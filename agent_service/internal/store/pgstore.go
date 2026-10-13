// Package store 的 PostgreSQL 实现：Agent / Session / Message 持久化。
//
// 依赖 pgx/v5 直连池化，方法集与内存版 Store 完全一致 ——
// service 层依赖 Repository 接口、orchestrate 依赖 SessionStore 窄接口，
// 替换实现时编排层零改动。
//
// 与内存版的有意差异：
//   - 不做 MaxSessionsPerUser / TTL 淘汰：持久化存储无内存压力，
//     会话历史完整保留（归档策略由 DB 层负责）
//   - MaxMessagesPerSession 裁剪不生效：消息全量落库，
//     RecentMessages 仍只取最近 N 条拼上下文
//
// 错误语义与内存版对齐，service 的 mapErr 分支无需改动：
//   - 租户内同名 Agent（唯一索引冲突）→ ErrConflict
//   - 查询无行 → ErrNotFound
//
// 并发一致性：AppendMessage 对 sessions 行 FOR UPDATE，
// 串行化同一会话的并发追加，保证 message_count / 标题回填不丢更新。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aegis/agent_service/internal/toolexec"
	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/logger"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgQueryTimeout 单条语句超时；所有 PG 查询显式带超时，防止慢查询拖垮连接池
const pgQueryTimeout = 5 * time.Second

// PgStore PostgreSQL 实现
type PgStore struct {
	pool *pgxpool.Pool
}

// 编译期保证两种实现都满足 Repository
var (
	_ Repository = (*Store)(nil)
	_ Repository = (*PgStore)(nil)
)

// NewPgStore 构造 PG 仓储。
// DSN 为空返回错误（调用方据此回退到内存实现）。
func NewPgStore(ctx context.Context, pgConf conf.PostgresConf) (*PgStore, error) {
	if pgConf.DSN == "" {
		return nil, errors.New("postgres DSN 未配置")
	}

	cfg, _ := pgxpool.ParseConfig(pgConf.DSN)
	if pgConf.MaxOpenConns > 0 {
		cfg.MaxConns = int32(pgConf.MaxOpenConns)
	}
	if pgConf.MaxIdleConns > 0 {
		cfg.MinConns = int32(pgConf.MaxIdleConns)
	}
	if pgConf.ConnMaxLifetime > 0 {
		cfg.MaxConnLifetime = pgConf.ConnMaxLifetime
	}
	if pgConf.QueryTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = pgConf.QueryTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgxpool 构造失败: %w", err)
	}

	// 立即 Ping：启动时发现问题比运行时静默失败好
	pingCtx, cancel := context.WithTimeout(ctx, pgQueryTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping 失败: %w", err)
	}

	return &PgStore{pool: pool}, nil
}

// Close 释放连接池
func (r *PgStore) Close() {
	if r != nil && r.pool != nil {
		r.pool.Close()
	}
}

// ---------- 内部辅助 ----------

// scoped 派生带超时的 ctx；调用方需 defer cancel()
func scoped(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, pgQueryTimeout)
}

// isUniqueViolation 唯一索引冲突（租户内同名 Agent）
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation
}

// agentCols Agent 全列清单，INSERT...RETURNING / SELECT 共用
const agentCols = `agent_id, name, description, avatar, system_prompt, model,
	temperature, max_tokens, tools, kb_ids, rag_enabled, rag_top_k, rag_score_threshold,
	uid, tenant, status, created_at, updated_at`

// scanAgentInto 从 Row/Rows 扫描一行 Agent。
// NUMERIC temperature 扫到 float64 再转 float32；时间转 UnixMilli。
func scanAgentInto(sc interface{ Scan(dest ...any) error }) (*agentv1.Agent, error) {
	a := &agentv1.Agent{}
	var created, updated time.Time
	var temp float64
	err := sc.Scan(&a.AgentId, &a.Name, &a.Description, &a.Avatar, &a.SystemPrompt, &a.Model,
		&temp, &a.MaxTokens, &a.Tools, &a.KbIds, &a.RagEnabled, &a.RagTopK, &a.RagScoreThreshold,
		&a.OwnerUid, &a.Tenant, &a.Status, &created, &updated)
	if err != nil {
		return nil, err
	}
	a.Temperature = float32(temp)
	a.CreatedAt = created.UnixMilli()
	a.UpdatedAt = updated.UnixMilli()
	return a, nil
}

// msgCols 消息读取列清单（不含 uid/session_id —— 调用方已有）
const msgCols = `msg_id, role, content, citations, prompt_tokens, completion_tokens,
	degraded, degrade_reason, created_at`

// scanMessageInto 从 Row/Rows 扫描一行消息。
// citations 为 JSONB：NULL → 无引用；非 NULL → 反序列化。
func scanMessageInto(sc interface{ Scan(dest ...any) error }) (*agentv1.ChatMessage, error) {
	m := &agentv1.ChatMessage{}
	var cites []byte
	var prompt, completion int32
	var created time.Time
	err := sc.Scan(&m.MsgId, &m.Role, &m.Content, &cites,
		&prompt, &completion, &m.Degraded, &m.DegradeReason, &created)
	if err != nil {
		return nil, err
	}
	if len(cites) > 0 {
		if err := json.Unmarshal(cites, &m.Citations); err != nil {
			return nil, fmt.Errorf("解析 citations 失败: %w", err)
		}
	}
	if prompt > 0 || completion > 0 {
		m.Usage = &agentv1.Usage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      prompt + completion,
		}
	}
	m.CreatedAt = created.UnixMilli()
	return m, nil
}

// ---------- Agent ----------

// CreateAgent 新建 Agent。
//
// name 在租户内唯一（uk_agents_tenant_name 部分唯一索引）——
// 允许重名会让用户难以分辨对话入口，且前端列表无法给出稳定的标识。
func (r *PgStore) CreateAgent(ctx context.Context, agent *agentv1.Agent) (int64, error) {
	if agent == nil || strings.TrimSpace(agent.Name) == "" {
		return 0, &ErrConflict{Msg: "Agent 名称不能为空"}
	}

	ctx, cancel := scoped(ctx)
	defer cancel()

	status := agent.Status
	if status == 0 {
		status = AgentStatusEnabled
	}
	name := strings.TrimSpace(agent.Name)
	tools := agent.Tools
	if tools == nil {
		tools = []string{}
	}
	kbIDs := agent.KbIds
	if kbIDs == nil {
		kbIDs = []int64{}
	}

	const sql = `INSERT INTO agents
		(name, description, avatar, system_prompt, model, temperature, max_tokens,
		 tools, kb_ids, rag_enabled, rag_top_k, rag_score_threshold, uid, tenant, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING agent_id, created_at, updated_at`

	var id int64
	var created, updated time.Time
	err := r.pool.QueryRow(ctx, sql,
		name, agent.Description, agent.Avatar, agent.SystemPrompt, agent.Model,
		float64(agent.Temperature), agent.MaxTokens, tools, kbIDs,
		agent.RagEnabled, agent.RagTopK, agent.RagScoreThreshold,
		agent.OwnerUid, agent.Tenant, status,
	).Scan(&id, &created, &updated)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, &ErrDuplicate{Msg: "同名 Agent 已存在: " + name}
		}
		return 0, fmt.Errorf("插入 agent 失败: %w", err)
	}
	return id, nil
}

// GetAgent 读取单个 Agent
func (r *PgStore) GetAgent(ctx context.Context, agentID int64) (*agentv1.Agent, error) {
	ctx, cancel := scoped(ctx)
	defer cancel()

	const sql = `SELECT ` + agentCols + ` FROM agents WHERE agent_id = $1 AND deleted_at IS NULL`
	a, err := scanAgentInto(r.pool.QueryRow(ctx, sql, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ErrNotFound{Msg: "Agent 不存在"}
	}
	if err != nil {
		return nil, fmt.Errorf("查询 agent 失败: %w", err)
	}
	return a, nil
}

// UpdateAgent 按掩码更新，RETURNING 全列返回更新后的完整行。
//
// mask 为空时更新全部可改字段 —— 与 proto 的 update_mask 语义一致；
// owner_uid / tenant 不在可改清单里 —— 归属创建时确定，不允许改写。
func (r *PgStore) UpdateAgent(ctx context.Context, agent *agentv1.Agent, mask []string) (*agentv1.Agent, error) {
	if agent == nil || agent.AgentId == 0 {
		return nil, &ErrNotFound{Msg: "Agent 不存在"}
	}

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

	var sets []string
	var args []any
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}

	// 与内存版一致：mask 含 name 但值为空时跳过，避免改名成空串
	if has("name") && strings.TrimSpace(agent.Name) != "" {
		add("name", strings.TrimSpace(agent.Name))
	}
	if has("description") {
		add("description", agent.Description)
	}
	if has("avatar") {
		add("avatar", agent.Avatar)
	}
	if has("system_prompt") {
		add("system_prompt", agent.SystemPrompt)
	}
	if has("model") {
		add("model", agent.Model)
	}
	if has("temperature") {
		add("temperature", float64(agent.Temperature))
	}
	if has("max_tokens") {
		add("max_tokens", agent.MaxTokens)
	}
	if has("tools") {
		tools := agent.Tools
		if tools == nil {
			tools = []string{}
		}
		add("tools", tools)
	}
	if has("kb_ids") {
		kbIDs := agent.KbIds
		if kbIDs == nil {
			kbIDs = []int64{}
		}
		add("kb_ids", kbIDs)
	}
	if has("rag_enabled") {
		add("rag_enabled", agent.RagEnabled)
	}
	if has("rag_top_k") {
		add("rag_top_k", agent.RagTopK)
	}
	if has("rag_score_threshold") {
		add("rag_score_threshold", agent.RagScoreThreshold)
	}
	if has("status") {
		add("status", agent.Status)
	}

	if len(sets) == 0 {
		// mask 过滤后没有可更新字段：返回现状
		return r.GetAgent(ctx, agent.AgentId)
	}

	ctx, cancel := scoped(ctx)
	defer cancel()

	// 触发 trg_agents_updated_at（UPDATE 语句本身即可）
	sql := `UPDATE agents SET ` + strings.Join(sets, ", ") +
		` WHERE agent_id = $` + strconv.Itoa(len(args)+1) + ` AND deleted_at IS NULL` +
		` RETURNING ` + agentCols
	args = append(args, agent.AgentId)

	a, err := scanAgentInto(r.pool.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ErrNotFound{Msg: "Agent 不存在"}
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, &ErrDuplicate{Msg: "同名 Agent 已存在: " + strings.TrimSpace(agent.Name)}
		}
		return nil, fmt.Errorf("更新 agent 失败: %w", err)
	}
	return a, nil
}

// DeleteAgent 删除 Agent 及其全部会话与消息（硬删除，与内存版语义一致）
func (r *PgStore) DeleteAgent(ctx context.Context, agentID int64) error {
	ctx, cancel := scoped(ctx)
	defer cancel()

	var one int
	err := r.pool.QueryRow(ctx, `SELECT 1 FROM agents WHERE agent_id = $1`, agentID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return &ErrNotFound{Msg: "Agent 不存在"}
	}
	if err != nil {
		return fmt.Errorf("检查 agent 失败: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开始事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 先删消息（经 sessions 反查），再删会话，最后删 Agent
	if _, err := tx.Exec(ctx,
		`DELETE FROM messages WHERE session_id IN (SELECT session_id FROM sessions WHERE agent_id = $1)`,
		agentID); err != nil {
		return fmt.Errorf("级联删除消息失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE agent_id = $1`, agentID); err != nil {
		return fmt.Errorf("级联删除会话失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agents WHERE agent_id = $1`, agentID); err != nil {
		return fmt.Errorf("删除 agent 失败: %w", err)
	}
	return tx.Commit(ctx)
}

// ListAgents 分页查询。
//
// ownerUID > 0 时只看自己的；否则需 admin（由 service 层校验后调用）。
func (r *PgStore) ListAgents(ctx context.Context, keyword string, ownerUID int64, tenant string, page, size int) ([]*agentv1.Agent, int) {
	var conds []string
	var args []any
	conds = append(conds, "deleted_at IS NULL")
	if keyword != "" {
		args = append(args, "%"+keyword+"%")
		conds = append(conds, fmt.Sprintf("(name ILIKE $%d OR description ILIKE $%d)", len(args), len(args)))
	}
	if tenant != "" {
		args = append(args, tenant)
		conds = append(conds, fmt.Sprintf("tenant = $%d", len(args)))
	}
	if ownerUID > 0 {
		args = append(args, ownerUID)
		conds = append(conds, fmt.Sprintf("uid = $%d", len(args)))
	}
	where := strings.Join(conds, " AND ")

	ctx, cancel := scoped(ctx)
	defer cancel()

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0
	}
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	args = append(args, size, (page-1)*size)
	rows, err := r.pool.Query(ctx,
		`SELECT `+agentCols+` FROM agents WHERE `+where+
			` ORDER BY created_at DESC, agent_id DESC LIMIT $`+strconv.Itoa(len(args)-1)+
			` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	var out []*agentv1.Agent
	for rows.Next() {
		a, err := scanAgentInto(rows)
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	return out, total
}

// ---------- Session ----------

// CreateSession 新建会话
func (r *PgStore) CreateSession(ctx context.Context, agentID int64, title string, id *commonv1.Identity) (*agentv1.Session, error) {
	ctx, cancel := scoped(ctx)
	defer cancel()

	var one int
	err := r.pool.QueryRow(ctx,
		`SELECT 1 FROM agents WHERE agent_id = $1 AND deleted_at IS NULL`, agentID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ErrNotFound{Msg: "Agent 不存在"}
	}
	if err != nil {
		return nil, fmt.Errorf("检查 agent 失败: %w", err)
	}

	const sql = `INSERT INTO sessions (agent_id, uid, tenant, title)
		VALUES ($1,$2,$3,$4)
		RETURNING session_id, message_count, created_at, updated_at`

	sess := &agentv1.Session{
		AgentId: agentID,
		Title:   strings.TrimSpace(title),
		Uid:     id.GetUid(),
		Tenant:  id.GetTenant(),
	}
	var created, updated time.Time
	err = r.pool.QueryRow(ctx, sql, agentID, sess.Uid, sess.Tenant, sess.Title).
		Scan(&sess.SessionId, &sess.MsgCount, &created, &updated)
	if err != nil {
		return nil, fmt.Errorf("插入 session 失败: %w", err)
	}
	sess.CreatedAt = created.UnixMilli()
	sess.UpdatedAt = updated.UnixMilli()
	return sess, nil
}

// sessionCols 会话读取列清单
const sessionCols = `session_id, agent_id, title, uid, tenant, message_count, created_at, updated_at`

func scanSessionInto(sc interface{ Scan(dest ...any) error }) (*agentv1.Session, error) {
	s := &agentv1.Session{}
	var created, updated time.Time
	err := sc.Scan(&s.SessionId, &s.AgentId, &s.Title, &s.Uid, &s.Tenant,
		&s.MsgCount, &created, &updated)
	if err != nil {
		return nil, err
	}
	s.CreatedAt = created.UnixMilli()
	s.UpdatedAt = updated.UnixMilli()
	return s, nil
}

// GetSession 读取会话
func (r *PgStore) GetSession(ctx context.Context, sessionID int64) (*agentv1.Session, error) {
	ctx, cancel := scoped(ctx)
	defer cancel()

	sess, err := scanSessionInto(r.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE session_id = $1`, sessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ErrNotFound{Msg: "会话不存在"}
	}
	if err != nil {
		return nil, fmt.Errorf("查询 session 失败: %w", err)
	}
	return sess, nil
}

// ListSessions 分页查询会话
func (r *PgStore) ListSessions(ctx context.Context, uid, agentID int64, tenant string, page, size int) ([]*agentv1.Session, int) {
	var conds []string
	var args []any
	if uid > 0 {
		args = append(args, uid)
		conds = append(conds, fmt.Sprintf("uid = $%d", len(args)))
	}
	if agentID > 0 {
		args = append(args, agentID)
		conds = append(conds, fmt.Sprintf("agent_id = $%d", len(args)))
	}
	if tenant != "" {
		args = append(args, tenant)
		conds = append(conds, fmt.Sprintf("tenant = $%d", len(args)))
	}
	where := "TRUE"
	if len(conds) > 0 {
		where = strings.Join(conds, " AND ")
	}

	ctx, cancel := scoped(ctx)
	defer cancel()

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0
	}
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	args = append(args, size, (page-1)*size)
	rows, err := r.pool.Query(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE `+where+
			` ORDER BY updated_at DESC, session_id DESC LIMIT $`+strconv.Itoa(len(args)-1)+
			` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	var out []*agentv1.Session
	for rows.Next() {
		sess, err := scanSessionInto(rows)
		if err != nil {
			continue
		}
		out = append(out, sess)
	}
	return out, total
}

// DeleteSession 删除会话及其消息（硬删除，与内存版语义一致）
func (r *PgStore) DeleteSession(ctx context.Context, sessionID int64) error {
	ctx, cancel := scoped(ctx)
	defer cancel()

	var one int
	err := r.pool.QueryRow(ctx, `SELECT 1 FROM sessions WHERE session_id = $1`, sessionID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return &ErrNotFound{Msg: "会话不存在"}
	}
	if err != nil {
		return fmt.Errorf("检查 session 失败: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开始事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE session_id = $1`, sessionID); err != nil {
		return fmt.Errorf("删除消息失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE session_id = $1`, sessionID); err != nil {
		return fmt.Errorf("删除 session 失败: %w", err)
	}
	return tx.Commit(ctx)
}

// TouchSession 更新最后活跃时间（不改动其他字段）。
// 失败仅记录日志 —— 活跃时间丢失不影响正确性。
func (r *PgStore) TouchSession(ctx context.Context, sessionID int64) {
	ctx, cancel := scoped(ctx)
	defer cancel()

	if _, err := r.pool.Exec(ctx,
		`UPDATE sessions SET updated_at = now() WHERE session_id = $1`, sessionID); err != nil {
		logger.Warn(ctx, "pg touch session 失败", "session_id", sessionID, "err", err)
	}
}

// ---------- Message ----------

// AppendMessage 追加消息并返回带 ID 的副本。
//
// 事务内三步：
//  1. FOR UPDATE 锁会话行 —— 串行化同一会话的并发追加，
//     保证 message_count 自增与标题回填不丢更新
//  2. INSERT 消息（citations 序列化为 JSONB）
//  3. UPDATE 会话计数 + 空标题回填首条用户消息摘要
func (r *PgStore) AppendMessage(ctx context.Context, sessionID int64, msg *agentv1.ChatMessage) (*agentv1.ChatMessage, error) {
	ctx, cancel := scoped(ctx)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开始事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var uid int64
	err = tx.QueryRow(ctx,
		`SELECT uid FROM sessions WHERE session_id = $1 FOR UPDATE`, sessionID).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ErrNotFound{Msg: "会话不存在"}
	}
	if err != nil {
		return nil, fmt.Errorf("锁定 session 失败: %w", err)
	}

	cp := cloneMessage(msg)

	// citations → JSONB + chunk id 数组（便于后续按引用溯源查询）
	var citesJSON []byte
	if len(cp.Citations) > 0 {
		citesJSON, err = json.Marshal(cp.Citations)
		if err != nil {
			return nil, fmt.Errorf("序列化 citations 失败: %w", err)
		}
	}
	chunkIDs := make([]string, 0, len(cp.Citations))
	for _, c := range cp.Citations {
		if c != nil && c.ChunkId != "" {
			chunkIDs = append(chunkIDs, c.ChunkId)
		}
	}

	var prompt, completion int32
	if cp.Usage != nil {
		prompt, completion = cp.Usage.PromptTokens, cp.Usage.CompletionTokens
	}

	const insertSQL = `INSERT INTO messages
		(session_id, uid, role, content, citations, cited_chunk_ids,
		 prompt_tokens, completion_tokens, degraded, degrade_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING msg_id, created_at`

	var created time.Time
	err = tx.QueryRow(ctx, insertSQL,
		sessionID, uid, cp.Role, cp.Content, citesJSON, chunkIDs,
		prompt, completion, cp.Degraded, cp.DegradeReason,
	).Scan(&cp.MsgId, &created)
	if err != nil {
		return nil, fmt.Errorf("插入消息失败: %w", err)
	}

	// total_tokens 累加 prompt+completion；标题留空时用首条用户消息摘要兜底，
	// 否则会话列表里全是"新会话"，用户无法分辨
	var totalTokens int64 = int64(prompt) + int64(completion)
	if _, err := tx.Exec(ctx, `UPDATE sessions SET
		message_count   = message_count + 1,
		total_tokens    = total_tokens + $2,
		last_message_at = now(),
		title           = CASE WHEN title = '' AND $3 = 'user' THEN $4 ELSE title END
		WHERE session_id = $1`,
		sessionID, totalTokens, cp.Role, summarize(cp.Content, 20)); err != nil {
		return nil, fmt.Errorf("更新会话计数失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交事务失败: %w", err)
	}

	cp.SessionId = sessionID
	cp.CreatedAt = created.UnixMilli()
	return cp, nil
}

// ListMessages 读取消息。
//
// direction: 1=正序，2/其它=倒序（默认取最近 N 条，更符合"看历史"的直觉）。
// 倒序时窗口本身按时间正序切出（OFFSET 语义），仅窗口内反序 —— 与内存版一致。
func (r *PgStore) ListMessages(ctx context.Context, sessionID int64, direction int32, page, size int) ([]*agentv1.ChatMessage, int) {
	ctx, cancel := scoped(ctx)
	defer cancel()

	var total int
	if err := r.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = $1`, sessionID).Scan(&total); err != nil {
		return nil, 0
	}

	page, size, start, end := paginate(page, size, total)
	if start >= total {
		return []*agentv1.ChatMessage{}, total
	}

	order := `created_at ASC, msg_id ASC`
	if direction != 1 {
		order = `created_at DESC, msg_id DESC`
	}
	rows, err := r.pool.Query(ctx,
		`SELECT `+msgCols+` FROM messages WHERE session_id = $1`+
			` ORDER BY `+order+` LIMIT $2 OFFSET $3`, sessionID, end-start, start)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	out := make([]*agentv1.ChatMessage, 0, end-start)
	for rows.Next() {
		m, err := scanMessageInto(rows)
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	if direction != 1 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, total
}

// RecentMessages 返回最近 n 条消息，按时间正序 —— 供上下文拼装使用。
// n <= 0 返回全部历史（与内存版一致）。
func (r *PgStore) RecentMessages(ctx context.Context, sessionID int64, n int) []*agentv1.ChatMessage {
	ctx, cancel := scoped(ctx)
	defer cancel()

	sql := `SELECT ` + msgCols + ` FROM messages WHERE session_id = $1` +
		` ORDER BY created_at DESC, msg_id DESC`
	args := []any{sessionID}
	if n > 0 {
		sql += ` LIMIT $2`
		args = append(args, n)
	}

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []*agentv1.ChatMessage
	for rows.Next() {
		m, err := scanMessageInto(rows)
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	// DESC 取最近 n 条后反转为正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// MessageCount 会话消息数（读会话表冗余计数，避免 COUNT 分区表）
func (r *PgStore) MessageCount(ctx context.Context, sessionID int64) int {
	ctx, cancel := scoped(ctx)
	defer cancel()

	var n int
	if err := r.pool.QueryRow(ctx,
		`SELECT message_count FROM sessions WHERE session_id = $1`, sessionID).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ---------- 统计 ----------

// Stats 统计信息（供健康检查暴露）
func (r *PgStore) Stats(ctx context.Context) map[string]int {
	ctx, cancel := scoped(ctx)
	defer cancel()

	var agents, sessions, msgs int
	err := r.pool.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM agents WHERE deleted_at IS NULL),
		(SELECT COUNT(*) FROM sessions),
		(SELECT COUNT(*) FROM messages)`).Scan(&agents, &sessions, &msgs)
	if err != nil {
		return map[string]int{"agents": 0, "sessions": 0, "messages": 0}
	}
	return map[string]int{"agents": agents, "sessions": sessions, "messages": msgs}
}

// ---------- ToolRegistry 实现 ----------

// GetTool 按名称查工具定义。
// 实现 toolexec.ToolRegistry，供 toolexec.NewHTTP 注入使用。
// 返回 ErrToolNotFound 时上层工具执行器会跳过本次调用（降级为纯对话）。
func (r *PgStore) GetTool(ctx context.Context, name string) (*toolexec.ToolDef, error) {
	ctx, cancel := scoped(ctx)
	defer cancel()

	var (
		def        toolexec.ToolDef
		schemaJSON string
	)
	err := r.pool.QueryRow(ctx, `
		SELECT name, COALESCE(description,''), COALESCE(type,'http'),
		       COALESCE(endpoint,''), COALESCE(method,'POST'),
		       COALESCE(params_schema::text,''), COALESCE(timeout_ms,5000)
		FROM tools WHERE name=$1 AND status=1`, name).
		Scan(&def.Name, &def.Description, &def.Type, &def.Endpoint, &def.Method, &schemaJSON, &def.TimeoutMS)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, toolexec.ErrToolNotFound
		}
		logger.Warn(ctx, "查询工具定义失败", "name", name, "err", err)
		return nil, fmt.Errorf("查询工具 %q 失败: %w", name, err)
	}
	def.ParamsSchema = schemaJSON
	return &def, nil
}

// ---------- Tool CRUD ----------

const toolCols = `tool_id, name, description, type, endpoint, method,
	params_schema, timeout_ms, status, created_at, updated_at`

func scanToolInto(sc interface{ Scan(dest ...any) error }) (*agentv1.Tool, error) {
	t := &agentv1.Tool{}
	var schemaJSON []byte
	var created, updated time.Time
	err := sc.Scan(&t.ToolId, &t.Name, &t.Description, &t.Type, &t.Endpoint, &t.Method,
		&schemaJSON, &t.TimeoutMs, &t.Status, &created, &updated)
	if err != nil {
		return nil, err
	}
	if len(schemaJSON) > 0 {
		t.ParamsSchema = string(schemaJSON)
	}
	t.CreatedAt = created.UnixMilli()
	t.UpdatedAt = updated.UnixMilli()
	return t, nil
}

// CreateTool 新建工具（name 全局唯一）
func (r *PgStore) CreateTool(ctx context.Context, tool *agentv1.Tool) (int64, error) {
	if tool == nil || strings.TrimSpace(tool.Name) == "" {
		return 0, &ErrConflict{Msg: "工具名称不能为空"}
	}
	ctx, cancel := scoped(ctx)
	defer cancel()

	status := tool.Status
	if status == 0 {
		status = 1
	}
	toolType := strings.TrimSpace(tool.Type)
	if toolType == "" {
		toolType = "http"
	}
	method := strings.ToUpper(strings.TrimSpace(tool.Method))
	if method == "" {
		method = "POST"
	}
	timeout := tool.TimeoutMs
	if timeout == 0 {
		timeout = 5000
	}

	// params_schema 是 JSONB，传 nil 时存 NULL
	var paramsSchema any = nil
	if tool.ParamsSchema != "" {
		paramsSchema = []byte(tool.ParamsSchema)
	}

	// tools 表 tool_id 无默认序列，用子查询分配
	const sql = `INSERT INTO tools
		(tool_id, name, description, type, endpoint, method, params_schema, timeout_ms, status)
		VALUES ((SELECT COALESCE(MAX(tool_id),0)+1 FROM tools), $1,$2,$3,$4,$5,$6::jsonb,$7,$8)
		RETURNING tool_id`

	var id int64
	err := r.pool.QueryRow(ctx, sql,
		strings.TrimSpace(tool.Name), tool.Description, toolType,
		tool.Endpoint, method, paramsSchema, timeout, status,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, &ErrDuplicate{Msg: "同名工具已存在: " + tool.Name}
		}
		return 0, fmt.Errorf("插入工具失败: %w", err)
	}
	return id, nil
}

// GetToolByID 按 ID 读取工具
func (r *PgStore) GetToolByID(ctx context.Context, toolID int64) (*agentv1.Tool, error) {
	ctx, cancel := scoped(ctx)
	defer cancel()

	t, err := scanToolInto(r.pool.QueryRow(ctx,
		`SELECT `+toolCols+` FROM tools WHERE tool_id = $1`, toolID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ErrNotFound{Msg: "工具不存在"}
	}
	if err != nil {
		return nil, fmt.Errorf("查询工具失败: %w", err)
	}
	return t, nil
}

// UpdateTool 按掩码更新工具
func (r *PgStore) UpdateTool(ctx context.Context, tool *agentv1.Tool, mask []string) (*agentv1.Tool, error) {
	if tool == nil || tool.ToolId == 0 {
		return nil, &ErrNotFound{Msg: "工具不存在"}
	}

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

	var sets []string
	var args []any
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}

	if has("name") && strings.TrimSpace(tool.Name) != "" {
		add("name", strings.TrimSpace(tool.Name))
	}
	if has("description") {
		add("description", tool.Description)
	}
	if has("type") {
		add("type", tool.Type)
	}
	if has("endpoint") {
		add("endpoint", tool.Endpoint)
	}
	if has("method") {
		add("method", tool.Method)
	}
	if has("params_schema") {
		if tool.ParamsSchema == "" {
			add("params_schema", nil)
		} else {
			add("params_schema", []byte(tool.ParamsSchema))
		}
	}
	if has("timeout_ms") {
		add("timeout_ms", tool.TimeoutMs)
	}
	if has("status") {
		add("status", tool.Status)
	}

	if len(sets) == 0 {
		return r.GetToolByID(ctx, tool.ToolId)
	}

	ctx, cancel := scoped(ctx)
	defer cancel()

	sql := `UPDATE tools SET ` + strings.Join(sets, ", ") +
		` WHERE tool_id = $` + strconv.Itoa(len(args)+1) +
		` RETURNING ` + toolCols
	args = append(args, tool.ToolId)

	t, err := scanToolInto(r.pool.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ErrNotFound{Msg: "工具不存在"}
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, &ErrDuplicate{Msg: "同名工具已存在: " + strings.TrimSpace(tool.Name)}
		}
		return nil, fmt.Errorf("更新工具失败: %w", err)
	}
	return t, nil
}

// DeleteTool 删除工具
func (r *PgStore) DeleteTool(ctx context.Context, toolID int64) error {
	ctx, cancel := scoped(ctx)
	defer cancel()

	tag, err := r.pool.Exec(ctx, `DELETE FROM tools WHERE tool_id = $1`, toolID)
	if err != nil {
		return fmt.Errorf("删除工具失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &ErrNotFound{Msg: "工具不存在"}
	}
	return nil
}

// ListTools 分页查询工具（keyword 模糊匹配 name/description + type 过滤）
func (r *PgStore) ListTools(ctx context.Context, keyword, toolType string, page, size int) ([]*agentv1.Tool, int) {
	var conds []string
	var args []any
	if keyword != "" {
		args = append(args, "%"+keyword+"%")
		conds = append(conds, fmt.Sprintf("(name ILIKE $%d OR description ILIKE $%d)", len(args), len(args)))
	}
	if toolType != "" {
		args = append(args, toolType)
		conds = append(conds, fmt.Sprintf("type = $%d", len(args)))
	}
	where := "TRUE"
	if len(conds) > 0 {
		where = strings.Join(conds, " AND ")
	}

	ctx, cancel := scoped(ctx)
	defer cancel()

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM tools WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0
	}
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	args = append(args, size, (page-1)*size)
	rows, err := r.pool.Query(ctx,
		`SELECT `+toolCols+` FROM tools WHERE `+where+
			` ORDER BY created_at DESC, tool_id DESC LIMIT $`+strconv.Itoa(len(args)-1)+
			` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	var out []*agentv1.Tool
	for rows.Next() {
		t, err := scanToolInto(rows)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, total
}

// ---------- Agent↔Tool 绑定 ----------

// BindAgentTools 全量替换：先清空该 agent 的绑定，再写入传入列表。
// 事务内执行确保原子性；agent_tools.agent_id 是 VARCHAR(64)，需 int64→string。
func (r *PgStore) BindAgentTools(ctx context.Context, agentID int64, bindings []*agentv1.AgentToolBinding) error {
	ctx, cancel := scoped(ctx)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开始事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	agentKey := strconv.FormatInt(agentID, 10)

	// 先清空
	if _, err := tx.Exec(ctx, `DELETE FROM agent_tools WHERE agent_id = $1`, agentKey); err != nil {
		return fmt.Errorf("清空 agent_tools 失败: %w", err)
	}

	if len(bindings) == 0 {
		return tx.Commit(ctx)
	}

	// 批量插入 —— 用 unnest 一次搞定
	var (
		toolNames []string
		enableds  []bool
		configs   [][]byte
	)
	for _, b := range bindings {
		if b == nil || strings.TrimSpace(b.ToolName) == "" {
			continue
		}
		toolNames = append(toolNames, strings.TrimSpace(b.ToolName))
		enableds = append(enableds, b.Enabled)
		var cfgJSON []byte
		if b.Config != "" {
			cfgJSON = []byte(b.Config)
		}
		configs = append(configs, cfgJSON)
	}

	if len(toolNames) > 0 {
		const sql = `INSERT INTO agent_tools (agent_id, tool_name, enabled, config)
			SELECT $1, x.t, x.e, x.c::jsonb
			FROM unnest($2::text[], $3::boolean[], $4::bytea[]) AS x(t, e, c)`
		if _, err := tx.Exec(ctx, sql, agentKey, toolNames, enableds, configs); err != nil {
			return fmt.Errorf("插入 agent_tools 失败: %w", err)
		}
	}

	return tx.Commit(ctx)
}

// UnbindAgentTools 解绑指定工具；toolNames 为空时解绑全部。
func (r *PgStore) UnbindAgentTools(ctx context.Context, agentID int64, toolNames []string) error {
	ctx, cancel := scoped(ctx)
	defer cancel()

	agentKey := strconv.FormatInt(agentID, 10)

	if len(toolNames) == 0 {
		_, err := r.pool.Exec(ctx, `DELETE FROM agent_tools WHERE agent_id = $1`, agentKey)
		return err
	}

	// ANY($1) 语法 —— 和 rag_service 里 agent_kbs 一致
	_, err := r.pool.Exec(ctx,
		`DELETE FROM agent_tools WHERE agent_id = $1 AND tool_name = ANY($2)`,
		agentKey, toolNames)
	if err != nil {
		return fmt.Errorf("解绑 agent_tools 失败: %w", err)
	}
	return nil
}

// ListAgentTools 返回该 agent 的全部工具绑定。
func (r *PgStore) ListAgentTools(ctx context.Context, agentID int64) ([]*agentv1.AgentToolBinding, error) {
	ctx, cancel := scoped(ctx)
	defer cancel()

	agentKey := strconv.FormatInt(agentID, 10)

	const sql = `SELECT tool_name, enabled, config, created_at FROM agent_tools
		WHERE agent_id = $1 ORDER BY created_at ASC`

	rows, err := r.pool.Query(ctx, sql, agentKey)
	if err != nil {
		return nil, fmt.Errorf("查询 agent_tools 失败: %w", err)
	}
	defer rows.Close()

	var out []*agentv1.AgentToolBinding
	for rows.Next() {
		var (
			toolName string
			enabled  bool
			cfgJSON  []byte
			created  time.Time
		)
		if err := rows.Scan(&toolName, &enabled, &cfgJSON, &created); err != nil {
			continue
		}
		b := &agentv1.AgentToolBinding{
			AgentId:   agentID,
			ToolName:  toolName,
			Enabled:   enabled,
			CreatedAt: created.UnixMilli(),
		}
		if len(cfgJSON) > 0 {
			b.Config = string(cfgJSON)
		}
		out = append(out, b)
	}
	return out, nil
}
