// Package store 提供 agent_service 的仓储：Agent / Session / Message。
//
// 双实现：
//
//	Store   内存版（mock 模式 / PG 不可用时的兜底）
//	PgStore PostgreSQL 版（生产链路，见 pgstore.go）
//
// 两者方法集一致（Repository 接口），service 层按接口接收，
// main 装配时选择实现，编排层零改动。
//
// ★ 并发安全原则（与 rag_service/internal/meta 一致）：
//
//	pb 生成的结构体里slice 字段（Agent.Tools/KbIds、ChatMessage.Citations）
//	共享底层数组。`cp := *x` 只复制指针，
//	外部任何append/原地改都会污染存储中的对象并触发 data race。
//	因此所有出参、入参一律经clone 函数深拷贝。
package store

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aegis/agent_service/internal/toolexec"
	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
)

// Agent 状态
const (
	AgentStatusDisabled int32 = 0
	AgentStatusEnabled  int32 = 1
)

// ---------- clone ----------

func cloneAgent(a *agentv1.Agent) *agentv1.Agent {
	if a == nil {
		return nil
	}
	cp := *a
	if a.Tools != nil {
		cp.Tools = append([]string(nil), a.Tools...)
	}
	if a.KbIds != nil {
		cp.KbIds = append([]int64(nil), a.KbIds...)
	}
	return &cp
}

func cloneSession(s *agentv1.Session) *agentv1.Session {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

func cloneUsage(u *agentv1.Usage) *agentv1.Usage {
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

func cloneCitation(c *agentv1.Citation) *agentv1.Citation {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

func cloneMessage(m *agentv1.ChatMessage) *agentv1.ChatMessage {
	if m == nil {
		return nil
	}
	cp := *m
	if m.Citations != nil {
		cp.Citations = make([]*agentv1.Citation, 0, len(m.Citations))
		for _, c := range m.Citations {
			cp.Citations = append(cp.Citations, cloneCitation(c))
		}
	}
	cp.Usage = cloneUsage(m.Usage)
	return &cp
}

func cloneAgents(in []*agentv1.Agent) []*agentv1.Agent {
	if in == nil {
		return nil
	}
	out := make([]*agentv1.Agent, 0, len(in))
	for _, a := range in {
		out = append(out, cloneAgent(a))
	}
	return out
}

func cloneMessages(in []*agentv1.ChatMessage) []*agentv1.ChatMessage {
	if in == nil {
		return nil
	}
	out := make([]*agentv1.ChatMessage, 0, len(in))
	for _, m := range in {
		out = append(out, cloneMessage(m))
	}
	return out
}

// ---------- 错误 ----------

// ErrNotFound 未找到
type ErrNotFound struct{ Msg string }

func (e *ErrNotFound) Error() string { return e.Msg }

// ErrConflict 冲突（如名称重复）
type ErrConflict struct{ Msg string }

func (e *ErrConflict) Error() string { return e.Msg }

// ErrDuplicate 唯一约束冲突（如同名 Agent / 同名工具已存在）。
//
// 与 ErrConflict 分开是因为二者对调用方的意义不同：重复是"换个名字再来"，
// 而状态冲突是"当前状态不允许该操作"。合并成一个码会让前端只能显示后者，
// 用户看不出自己只是重了名。两者 HTTP 状态码同为 409，不影响网关映射。
type ErrDuplicate struct{ Msg string }

func (e *ErrDuplicate) Error() string { return e.Msg }

// ErrForbidden 无权操作
type ErrForbidden struct{ Msg string }

func (e *ErrForbidden) Error() string { return e.Msg }

// ---------- Repository 仓储接口 ----------

// Repository 仓储接口：内存版（Store）与 PG 版（PgStore）共同实现。
//
// service 层只依赖本接口；orchestrate 用更窄的 SessionStore（两方法）。
// ctx 贯穿到 PG 查询：请求取消/超时能及时中断数据库操作。
type Repository interface {
	// Agent
	CreateAgent(ctx context.Context, agent *agentv1.Agent) (int64, error)
	GetAgent(ctx context.Context, agentID int64) (*agentv1.Agent, error)
	UpdateAgent(ctx context.Context, agent *agentv1.Agent, mask []string) (*agentv1.Agent, error)
	DeleteAgent(ctx context.Context, agentID int64) error
	ListAgents(ctx context.Context, keyword string, ownerUID int64, tenant string, page, size int) ([]*agentv1.Agent, int)

	// Session
	CreateSession(ctx context.Context, agentID int64, title string, id *commonv1.Identity) (*agentv1.Session, error)
	GetSession(ctx context.Context, sessionID int64) (*agentv1.Session, error)
	ListSessions(ctx context.Context, uid, agentID int64, tenant string, page, size int) ([]*agentv1.Session, int)
	DeleteSession(ctx context.Context, sessionID int64) error
	TouchSession(ctx context.Context, sessionID int64)

	// Message
	AppendMessage(ctx context.Context, sessionID int64, msg *agentv1.ChatMessage) (*agentv1.ChatMessage, error)
	ListMessages(ctx context.Context, sessionID int64, direction int32, page, size int) ([]*agentv1.ChatMessage, int)
	RecentMessages(ctx context.Context, sessionID int64, n int) []*agentv1.ChatMessage
	MessageCount(ctx context.Context, sessionID int64) int

	// Tool CRUD
	CreateTool(ctx context.Context, tool *agentv1.Tool) (int64, error)
	GetToolByID(ctx context.Context, toolID int64) (*agentv1.Tool, error)
	UpdateTool(ctx context.Context, tool *agentv1.Tool, mask []string) (*agentv1.Tool, error)
	DeleteTool(ctx context.Context, toolID int64) error
	ListTools(ctx context.Context, keyword, toolType string, page, size int) ([]*agentv1.Tool, int)

	// Agent↔Tool 绑定
	BindAgentTools(ctx context.Context, agentID int64, bindings []*agentv1.AgentToolBinding) error
	UnbindAgentTools(ctx context.Context, agentID int64, toolNames []string) error
	ListAgentTools(ctx context.Context, agentID int64) ([]*agentv1.AgentToolBinding, error)

	// 其他
	Stats(ctx context.Context) map[string]int
	Close()
}

// ---------- Store 内存仓储 ----------

// Store 内存实现，全部操作在 mu 下串行。
type Store struct {
	mu sync.RWMutex

	agents   map[int64]*agentv1.Agent
	sessions map[int64]*agentv1.Session
	// messages 按 session_id 分组，元素按时间升序
	messages map[int64][]*agentv1.ChatMessage
	tools    map[int64]*agentv1.Tool
	// agentTools[agentID][toolName] = binding
	agentTools map[int64]map[string]*agentv1.AgentToolBinding

	nextAgentID   int64
	nextSessionID int64
	nextMsgID     int64
	nextToolID    int64

	// 用户→其会话 ID 列表，用于按用户淘汰
	userSessions map[int64][]int64

	maxMsgsPerSession  int
	maxSessionsPerUser int
	ttl                time.Duration
}

// Options 仓储参数
type Options struct {
	MaxMessagesPerSession int
	MaxSessionsPerUser    int
	SessionTTL            time.Duration
}

// New 构造内存仓储
func New(opt Options) *Store {
	if opt.MaxMessagesPerSession <= 0 {
		opt.MaxMessagesPerSession = 500
	}
	if opt.MaxSessionsPerUser <= 0 {
		opt.MaxSessionsPerUser = 200
	}
	if opt.SessionTTL <= 0 {
		opt.SessionTTL = 7 * 24 * time.Hour
	}
	return &Store{
		agents:             make(map[int64]*agentv1.Agent),
		sessions:           make(map[int64]*agentv1.Session),
		messages:           make(map[int64][]*agentv1.ChatMessage),
		tools:              make(map[int64]*agentv1.Tool),
		agentTools:         make(map[int64]map[string]*agentv1.AgentToolBinding),
		userSessions:       make(map[int64][]int64),
		nextAgentID:        1000,
		nextSessionID:      5000,
		nextMsgID:          90000,
		nextToolID:         1,
		maxMsgsPerSession:  opt.MaxMessagesPerSession,
		maxSessionsPerUser: opt.MaxSessionsPerUser,
		ttl:                opt.SessionTTL,
	}
}

// ---------- Agent ----------

// CreateAgent 新建 Agent。
//
// name 在租户内唯一 —— 允许重名会让用户难以分辨对话入口，
// 且前端列表无法给出稳定的标识。
func (s *Store) CreateAgent(ctx context.Context, agent *agentv1.Agent) (int64, error) {
	if agent == nil || strings.TrimSpace(agent.Name) == "" {
		return 0, &ErrConflict{Msg: "Agent 名称不能为空"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	name := strings.TrimSpace(agent.Name)
	tenant := agent.Tenant
	for _, a := range s.agents {
		if a.Tenant == tenant && a.Name == name {
			return 0, &ErrDuplicate{Msg: "同名 Agent 已存在: " + name}
		}
	}

	now := time.Now().UnixMilli()
	s.nextAgentID++
	cp := cloneAgent(agent)
	cp.AgentId = s.nextAgentID
	cp.Name = name
	if cp.Status == 0 {
		cp.Status = AgentStatusEnabled
	}
	cp.CreatedAt = now
	cp.UpdatedAt = now

	s.agents[cp.AgentId] = cp
	return cp.AgentId, nil
}

// GetAgent 读取单个 Agent
func (s *Store) GetAgent(ctx context.Context, agentID int64) (*agentv1.Agent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.agents[agentID]
	if !ok {
		return nil, &ErrNotFound{Msg: "Agent 不存在"}
	}
	return cloneAgent(a), nil
}

// UpdateAgent 按掩码更新。
//
// mask 为空时更新全部可改字段 —— 与 proto 的 update_mask 语义一致。
func (s *Store) UpdateAgent(ctx context.Context, agent *agentv1.Agent, mask []string) (*agentv1.Agent, error) {
	if agent == nil || agent.AgentId == 0 {
		return nil, &ErrNotFound{Msg: "Agent 不存在"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	old, ok := s.agents[agent.AgentId]
	if !ok {
		return nil, &ErrNotFound{Msg: "Agent 不存在"}
	}

	cp := cloneAgent(old)
	has := func(f string) bool {
		if len(mask) == 0 {
			return true
		}
		for _, m := range mask {
			if m == f || m == "*" {
				return true
			}
		}
		return false
	}

	// 名称唯一性检查
	if has("name") && agent.Name != "" && agent.Name != old.Name {
		newName := strings.TrimSpace(agent.Name)
		for id, a := range s.agents {
			if id != old.AgentId && a.Tenant == old.Tenant && a.Name == newName {
				return nil, &ErrDuplicate{Msg: "同名 Agent 已存在: " + newName}
			}
		}
		cp.Name = newName
	}

	if has("description") {
		cp.Description = agent.Description
	}
	if has("avatar") {
		cp.Avatar = agent.Avatar
	}
	if has("system_prompt") {
		cp.SystemPrompt = agent.SystemPrompt
	}
	if has("model") {
		cp.Model = agent.Model
	}
	if has("temperature") {
		cp.Temperature = agent.Temperature
	}
	if has("max_tokens") {
		cp.MaxTokens = agent.MaxTokens
	}
	if has("tools") {
		cp.Tools = append([]string(nil), agent.Tools...)
	}
	if has("kb_ids") {
		cp.KbIds = append([]int64(nil), agent.KbIds...)
	}
	if has("rag_enabled") {
		cp.RagEnabled = agent.RagEnabled
	}
	if has("rag_top_k") {
		cp.RagTopK = agent.RagTopK
	}
	if has("rag_score_threshold") {
		cp.RagScoreThreshold = agent.RagScoreThreshold
	}
	if has("status") {
		cp.Status = agent.Status
	}
	cp.UpdatedAt = time.Now().UnixMilli()

	s.agents[cp.AgentId] = cp
	return cloneAgent(cp), nil
}

// DeleteAgent 删除 Agent 及其全部会话与消息
func (s *Store) DeleteAgent(ctx context.Context, agentID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return &ErrNotFound{Msg: "Agent 不存在"}
	}
	delete(s.agents, agentID)

	// 级联清理会话与消息，避免留下孤儿数据占内存
	for sid, sess := range s.sessions {
		if sess.AgentId == agentID {
			delete(s.messages, sid)
			delete(s.sessions, sid)
			s.removeUserSessionLocked(sess.Uid, sid)
		}
	}
	return nil
}

// ListAgents 分页查询。
//
// ownerUID > 0 时只看自己的；否则需admin（由service 层校验后调用）。
func (s *Store) ListAgents(ctx context.Context, keyword string, ownerUID int64, tenant string, page, size int) ([]*agentv1.Agent, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	kw := strings.ToLower(strings.TrimSpace(keyword))
	var all []*agentv1.Agent
	for _, a := range s.agents {
		if tenant != "" && a.Tenant != tenant {
			continue
		}
		if ownerUID > 0 && a.OwnerUid != ownerUID {
			continue
		}
		if kw != "" &&
			!strings.Contains(strings.ToLower(a.Name), kw) &&
			!strings.Contains(strings.ToLower(a.Description), kw) {
			continue
		}
		all = append(all, cloneAgent(a))
	}

	// 按创建时间倒序，新的在前
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreatedAt == all[j].CreatedAt {
			return all[i].AgentId > all[j].AgentId
		}
		return all[i].CreatedAt > all[j].CreatedAt
	})

	total := len(all)
	page, size, start, end := paginate(page, size, total)
	if start >= total {
		return []*agentv1.Agent{}, total
	}
	return all[start:end], total
}

// ---------- Session ----------

// CreateSession 新建会话
func (s *Store) CreateSession(ctx context.Context, agentID int64, title string, id *commonv1.Identity) (*agentv1.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return nil, &ErrNotFound{Msg: "Agent 不存在"}
	}

	now := time.Now().UnixMilli()
	s.nextSessionID++
	sess := &agentv1.Session{
		SessionId: s.nextSessionID,
		AgentId:   agentID,
		Title:     strings.TrimSpace(title),
		Uid:       id.GetUid(),
		Tenant:    id.GetTenant(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.sessions[sess.SessionId] = sess
	s.appendUserSessionLocked(sess.Uid, sess.SessionId)
	s.evictUserSessionsLocked(sess.Uid)
	s.evictExpiredLocked()

	return cloneSession(sess), nil
}

// GetSession 读取会话
func (s *Store) GetSession(ctx context.Context, sessionID int64) (*agentv1.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil, &ErrNotFound{Msg: "会话不存在"}
	}
	return cloneSession(sess), nil
}

// ListSessions 分页查询会话
func (s *Store) ListSessions(ctx context.Context, uid, agentID int64, tenant string, page, size int) ([]*agentv1.Session, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var all []*agentv1.Session
	for _, sess := range s.sessions {
		if uid > 0 && sess.Uid != uid {
			continue
		}
		if agentID > 0 && sess.AgentId != agentID {
			continue
		}
		if tenant != "" && sess.Tenant != tenant {
			continue
		}
		all = append(all, cloneSession(sess))
	}

	// 按最后活跃时间倒序
	sort.Slice(all, func(i, j int) bool {
		if all[i].UpdatedAt == all[j].UpdatedAt {
			return all[i].SessionId > all[j].SessionId
		}
		return all[i].UpdatedAt > all[j].UpdatedAt
	})

	total := len(all)
	_, _, start, end := paginate(page, size, total)
	if start >= total {
		return []*agentv1.Session{}, total
	}
	return all[start:end], total
}

// DeleteSession 删除会话及其消息
func (s *Store) DeleteSession(ctx context.Context, sessionID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return &ErrNotFound{Msg: "会话不存在"}
	}
	delete(s.sessions, sessionID)
	delete(s.messages, sessionID)
	s.removeUserSessionLocked(sess.Uid, sessionID)
	return nil
}

// TouchSession 更新最后活跃时间（不改动其他字段）
func (s *Store) TouchSession(ctx context.Context, sessionID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.UpdatedAt = time.Now().UnixMilli()
	}
}

// ---------- Message ----------

// AppendMessage 追加消息并返回带 ID 的副本。
//
// 追加后按需裁剪：超出上限时丢弃最旧的消息。
func (s *Store) AppendMessage(ctx context.Context, sessionID int64, msg *agentv1.ChatMessage) (*agentv1.ChatMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[sessionID]; !ok {
		return nil, &ErrNotFound{Msg: "会话不存在"}
	}

	s.nextMsgID++
	cp := cloneMessage(msg)
	cp.MsgId = s.nextMsgID
	cp.SessionId = sessionID
	cp.CreatedAt = time.Now().UnixMilli()

	s.messages[sessionID] = append(s.messages[sessionID], cp)

	// 裁剪：保留最近 N 条。
	// 从头部丢弃 —— 开头的历史对模型价值最低，而结尾最关键。
	if n := len(s.messages[sessionID]); n > s.maxMsgsPerSession {
		s.messages[sessionID] = s.messages[sessionID][n-s.maxMsgsPerSession:]
	}

	if sess, ok := s.sessions[sessionID]; ok {
		sess.MsgCount = int32(len(s.messages[sessionID]))
		sess.UpdatedAt = cp.CreatedAt
		// 标题留空时用首条用户消息摘要兜底，
		// 否则会话列表里全是"新会话"，用户无法分辨。
		if sess.Title == "" && cp.Role == "user" {
			sess.Title = summarize(cp.Content, 20)
		}
	}

	return cloneMessage(cp), nil
}

// ListMessages 读取消息。
//
// direction: 1=正序，2/其它=倒序（默认取最近 N 条，更符合"看历史"的直觉）
func (s *Store) ListMessages(ctx context.Context, sessionID int64, direction int32, page, size int) ([]*agentv1.ChatMessage, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	msgs := s.messages[sessionID]
	total := len(msgs)
	page, size, start, end := paginate(page, size, total)
	if start >= total {
		return []*agentv1.ChatMessage{}, total
	}
	win := msgs[start:end]

	out := cloneMessages(win)
	if direction == 1 {
		return out, total
	}
	// 倒序：窗口内反序，但窗口本身按时间正序切出
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, total
}

// RecentMessages 返回最近 n 条消息，按时间正序 —— 供上下文拼装使用。
//
// ★ 必须在锁内完成深拷贝后返回，不能把内部切片交给调用方：
//
//	编排层会遍历并转成模型消息，AppendMessage 又在另一条路径上并发写入。
func (s *Store) RecentMessages(ctx context.Context, sessionID int64, n int) []*agentv1.ChatMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	msgs := s.messages[sessionID]
	if len(msgs) == 0 {
		return nil
	}
	if n > 0 && len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	return cloneMessages(msgs)
}

// MessageCount 会话消息数
func (s *Store) MessageCount(ctx context.Context, sessionID int64) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.messages[sessionID])
}

// ---------- 淘汰 ----------

func (s *Store) appendUserSessionLocked(uid, sessionID int64) {
	if uid <= 0 {
		return
	}
	s.userSessions[uid] = append(s.userSessions[uid], sessionID)
}

func (s *Store) removeUserSessionLocked(uid, sessionID int64) {
	if uid <= 0 {
		return
	}
	list := s.userSessions[uid]
	for i, v := range list {
		if v == sessionID {
			s.userSessions[uid] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

// evictUserSessionsLocked 按最后活跃时间淘汰该用户最旧的会话。
//
// 内存实现必须设上限：会话只增不减会让长时间运行的进程
// 最终被历史会话吃光内存。
func (s *Store) evictUserSessionsLocked(uid int64) {
	if uid <= 0 || len(s.userSessions[uid]) <= s.maxSessionsPerUser {
		return
	}
	list := s.userSessions[uid]
	// 按最后活跃时间排序，保留最近的
	sort.Slice(list, func(i, j int) bool {
		si, sj := s.sessions[list[i]], s.sessions[list[j]]
		if si == nil || sj == nil {
			return false
		}
		return si.UpdatedAt > sj.UpdatedAt
	})
	keep := list[:s.maxSessionsPerUser]
	drop := list[s.maxSessionsPerUser:]
	for _, sid := range drop {
		if sess, ok := s.sessions[sid]; ok && sess.Uid == uid {
			delete(s.sessions, sid)
			delete(s.messages, sid)
		}
	}
	s.userSessions[uid] = keep
}

// evictExpiredLocked 清理空闲超TTL 的会话
func (s *Store) evictExpiredLocked() {
	deadline := time.Now().Add(-s.ttl).UnixMilli()
	for sid, sess := range s.sessions {
		if sess.UpdatedAt < deadline {
			delete(s.sessions, sid)
			delete(s.messages, sid)
			s.removeUserSessionLocked(sess.Uid, sid)
		}
	}
}

// StartJanitor 定期清理过期会话，返回停止函数。
//
// ★ 返回的函数可重复调用（幂等）。
//
//	它在 main 里既是 Register 失败时的清理钩子，
//	又是正常退出时的清理钩子，两条路径都可能触发；
//	close 已关闭的 channel 会 panic，
//	而 panic 发生在优雅退出阶段会掩盖真正的错误信息。
func (s *Store) StartJanitor(interval time.Duration) func() {
	if interval <= 0 {
		interval = time.Hour
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.mu.Lock()
				s.evictExpiredLocked()
				s.mu.Unlock()
			}
		}
	}()

	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}

// Close 释放资源（内存版无外部资源，no-op；与 PgStore 接口对齐）
func (s *Store) Close() {}

// GetTool 内存版不支持工具定义，一律返回 ErrToolNotFound。
// 实现 toolexec.ToolRegistry——mock/内存模式下工具执行自动降级为"跳过"。
func (s *Store) GetTool(ctx context.Context, name string) (*toolexec.ToolDef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tools {
		if t.GetName() == name && t.GetStatus() == 1 {
			return &toolexec.ToolDef{
				Name:         t.GetName(),
				Description:  t.GetDescription(),
				Type:         t.GetType(),
				Endpoint:     t.GetEndpoint(),
				Method:       t.GetMethod(),
				ParamsSchema: t.GetParamsSchema(),
				TimeoutMS:    int(t.GetTimeoutMs()),
			}, nil
		}
	}
	return nil, toolexec.ErrToolNotFound
}

// cloneTool 深拷贝 Tool（pb 结构体无 slice 字段，浅拷贝即可）
func cloneTool(t *agentv1.Tool) *agentv1.Tool {
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

// CreateTool 新建工具（name 全局唯一）
func (s *Store) CreateTool(ctx context.Context, tool *agentv1.Tool) (int64, error) {
	if tool == nil || strings.TrimSpace(tool.Name) == "" {
		return 0, &ErrConflict{Msg: "工具名称不能为空"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tools {
		if t.Name == tool.Name {
			return 0, &ErrDuplicate{Msg: "同名工具已存在: " + tool.Name}
		}
	}
	now := time.Now().UnixMilli()
	s.nextToolID++
	cp := cloneTool(tool)
	cp.ToolId = s.nextToolID
	cp.Name = strings.TrimSpace(tool.Name)
	if cp.Type == "" {
		cp.Type = "http"
	}
	if cp.Method == "" {
		cp.Method = "POST"
	}
	if cp.TimeoutMs == 0 {
		cp.TimeoutMs = 5000
	}
	if cp.Status == 0 {
		cp.Status = 1
	}
	cp.CreatedAt = now
	cp.UpdatedAt = now
	s.tools[cp.ToolId] = cp
	return cp.ToolId, nil
}

// GetToolByID 按 ID 读取
func (s *Store) GetToolByID(ctx context.Context, toolID int64) (*agentv1.Tool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tools[toolID]
	if !ok {
		return nil, &ErrNotFound{Msg: "工具不存在"}
	}
	return cloneTool(t), nil
}

// UpdateTool 按掩码更新
func (s *Store) UpdateTool(ctx context.Context, tool *agentv1.Tool, mask []string) (*agentv1.Tool, error) {
	if tool == nil || tool.ToolId == 0 {
		return nil, &ErrNotFound{Msg: "工具不存在"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.tools[tool.ToolId]
	if !ok {
		return nil, &ErrNotFound{Msg: "工具不存在"}
	}
	cp := cloneTool(old)
	has := func(f string) bool {
		if len(mask) == 0 {
			return true
		}
		for _, m := range mask {
			if m == f || m == "*" {
				return true
			}
		}
		return false
	}
	if has("name") && strings.TrimSpace(tool.Name) != "" && tool.Name != old.Name {
		newName := strings.TrimSpace(tool.Name)
		for id, t := range s.tools {
			if id != old.ToolId && t.Name == newName {
				return nil, &ErrDuplicate{Msg: "同名工具已存在: " + newName}
			}
		}
		cp.Name = newName
	}
	if has("description") {
		cp.Description = tool.Description
	}
	if has("type") {
		cp.Type = tool.Type
	}
	if has("endpoint") {
		cp.Endpoint = tool.Endpoint
	}
	if has("method") {
		cp.Method = tool.Method
	}
	if has("params_schema") {
		cp.ParamsSchema = tool.ParamsSchema
	}
	if has("timeout_ms") {
		cp.TimeoutMs = tool.TimeoutMs
	}
	if has("status") {
		cp.Status = tool.Status
	}
	cp.UpdatedAt = time.Now().UnixMilli()
	s.tools[cp.ToolId] = cp
	return cloneTool(cp), nil
}

// DeleteTool 按 ID 删除
func (s *Store) DeleteTool(ctx context.Context, toolID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tools[toolID]; !ok {
		return &ErrNotFound{Msg: "工具不存在"}
	}
	delete(s.tools, toolID)
	return nil
}

// ListTools 分页查询（支持 keyword 模糊匹配 + type 过滤）
func (s *Store) ListTools(ctx context.Context, keyword, toolType string, page, size int) ([]*agentv1.Tool, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	kw := strings.ToLower(strings.TrimSpace(keyword))
	var all []*agentv1.Tool
	for _, t := range s.tools {
		if toolType != "" && t.Type != toolType {
			continue
		}
		if kw != "" &&
			!strings.Contains(strings.ToLower(t.Name), kw) &&
			!strings.Contains(strings.ToLower(t.Description), kw) {
			continue
		}
		all = append(all, cloneTool(t))
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreatedAt == all[j].CreatedAt {
			return all[i].ToolId > all[j].ToolId
		}
		return all[i].CreatedAt > all[j].CreatedAt
	})
	total := len(all)
	_, _, start, end := paginate(page, size, total)
	if start >= total {
		return []*agentv1.Tool{}, total
	}
	return all[start:end], total
}

// Stats 统计信息（供健康检查暴露）
func (s *Store) Stats(ctx context.Context) map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	msgCount := 0
	for _, ms := range s.messages {
		msgCount += len(ms)
	}
	return map[string]int{
		"agents":   len(s.agents),
		"sessions": len(s.sessions),
		"messages": msgCount,
	}
}

// ---------- Agent↔Tool 绑定 ----------

// BindAgentTools 全量替换：先清空该 agent 的绑定，再写入传入列表。
// 空列表 = 清空该 agent 的全部工具绑定。
func (s *Store) BindAgentTools(_ context.Context, agentID int64, bindings []*agentv1.AgentToolBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 先清空
	s.agentTools[agentID] = nil
	if len(bindings) == 0 {
		return nil
	}

	m := make(map[string]*agentv1.AgentToolBinding, len(bindings))
	now := time.Now().UnixMilli()
	for _, b := range bindings {
		if b == nil || strings.TrimSpace(b.ToolName) == "" {
			continue
		}
		enabled := b.Enabled
		m[strings.TrimSpace(b.ToolName)] = &agentv1.AgentToolBinding{
			AgentId:   agentID,
			ToolName:  strings.TrimSpace(b.ToolName),
			Enabled:   enabled,
			Config:    b.Config,
			CreatedAt: now,
		}
	}
	if len(m) > 0 {
		s.agentTools[agentID] = m
	}
	return nil
}

// UnbindAgentTools 解绑指定工具；toolNames 为空时解绑全部。
func (s *Store) UnbindAgentTools(_ context.Context, agentID int64, toolNames []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	m := s.agentTools[agentID]
	if m == nil {
		return nil
	}
	if len(toolNames) == 0 {
		delete(s.agentTools, agentID)
		return nil
	}
	for _, name := range toolNames {
		delete(m, name)
	}
	if len(m) == 0 {
		delete(s.agentTools, agentID)
	}
	return nil
}

// ListAgentTools 返回该 agent 的全部工具绑定。
func (s *Store) ListAgentTools(_ context.Context, agentID int64) ([]*agentv1.AgentToolBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	m := s.agentTools[agentID]
	if len(m) == 0 {
		return []*agentv1.AgentToolBinding{}, nil
	}
	out := make([]*agentv1.AgentToolBinding, 0, len(m))
	for _, b := range m {
		out = append(out, cloneAgentToolBinding(b))
	}
	return out, nil
}

func cloneAgentToolBinding(b *agentv1.AgentToolBinding) *agentv1.AgentToolBinding {
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

// ---------- 辅助 ----------

// paginate 计算分页窗口。
//
// 返回规整后的 page/size 与 [start, end) 区间；
// end 已做边界收敛，调用方只需判断 start 是否越界。
func paginate(page, size, total int) (p, s, start, end int) {
	p, s = page, size
	if p <= 0 {
		p = 1
	}
	if s <= 0 {
		s = 20
	}
	if s > 200 {
		s = 200
	}
	start = (p - 1) * s
	if start < 0 {
		start = 0
	}
	end = start + s
	if end > total {
		end = total
	}
	return
}

// summarize 截断标题到 n 个字符（按 rune，避免切坏UTF-8）
func summarize(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
