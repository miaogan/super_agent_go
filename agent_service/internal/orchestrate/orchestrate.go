// Package orchestrate 实现 agent_service 的对话编排。
//
// 一次对话的完整链路：
//
//	用户提问 → 组装历史 → RAG 检索 → 拼装上下文 → 调 llm_service → 落库 → 返回
//
// 三条设计原则：
//
//  1. RAG 永远不阻断对话。检索失败/无命中时降级为纯 LLM 回答，
//     并通过 degraded / degrade_reason 如实告知，绝不静默假装有资料。
//
//  2. 引用必须可溯源。命中的每条切片都编号并映射回 doc/page，
//     模型回答里的 [n] 才能对应到真实出处。
//
//  3. 流式与非流式共用同一套上下文拼装。
//     两份逻辑分叉会导致"流式能看到资料、非流式看不到"这类难查的不一致。
package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/logger"
)

// RAGRetriever 检索能力抽象，便于测试替身注入
type RAGRetriever interface {
	Retrieve(ctx context.Context, req *ragv1.RetrieveReq) (*ragv1.RetrieveResp, error)
}

// AgentKBMounter 按 agent 拉取其绑定的知识库 ID。
//
// 绑定真相源在 rag_service 的 agent_kbs 表（见 BindAgentKB/ListAgentKB）。
// 可选能力：编排器未注入时不检索绑定，行为与旧版一致。
type AgentKBMounter interface {
	ListAgentKB(ctx context.Context, agentID int64) ([]int64, error)
}

// ChatClient 模型调用抽象，便于测试替身注入
type ChatClient interface {
	Chat(ctx context.Context, req *llmv1.ChatReq) (*llmv1.ChatResp, error)
	ChatStream(ctx context.Context, req *llmv1.ChatReq) (llmservice.LLMService_ChatStreamClient, error)
}

// SessionStore 会话与消息持久化能力
type SessionStore interface {
	RecentMessages(ctx context.Context, sessionID int64, n int) []*agentv1.ChatMessage
	AppendMessage(ctx context.Context, sessionID int64, msg *agentv1.ChatMessage) (*agentv1.ChatMessage, error)
}

// Options 编排器参数
type Options struct {
	LLM    ChatClient
	RAG    RAGRetriever
	Store  SessionStore
	Agent  conf.AgentConf
	RAGCfg conf.RAGConf
	// KBMount agent 知识库绑定解析器（可选，来自 rag_service）
	KBMount AgentKBMounter
	// EnableRAG 全局开关
	EnableRAG bool
	// ToolExec 工具执行器（HTTP 实现）。
	// nil 时 tool_call 只收集不执行（降级为纯对话）。
	ToolExec ToolExecutor
	// MaxToolRounds 最大工具调用轮数，默认 5。
	MaxToolRounds int
}

// ToolExecutor 工具执行器（定义在 toolexec 包，这里抽接口便于 orchestrate 依赖）
type ToolExecutor interface {
	Execute(ctx context.Context, toolName string, argumentsJSON string) (resultJSON string, err error)
}

// Orchestrator 对话编排器
type Orchestrator struct {
	llm     ChatClient
	rag     RAGRetriever
	store   SessionStore
	kbMount AgentKBMounter
	tool    ToolExecutor
	maxRnds int
	opt     Options
}

// New 构造编排器
func New(opt Options) *Orchestrator {
	maxR := opt.MaxToolRounds
	if maxR <= 0 {
		maxR = 5
	}
	return &Orchestrator{
		llm:     opt.LLM,
		rag:     opt.RAG,
		store:   opt.Store,
		kbMount: opt.KBMount,
		tool:    opt.ToolExec,
		maxRnds: maxR,
		opt:     opt,
	}
}

// AgentPlan 一次对话的运行计划（由 Agent 配置 + 请求覆盖项合成）
type AgentPlan struct {
	SystemPrompt    string
	Model           string
	Temperature     float32
	MaxTokens       int32
	Tools           []string
	UseRAG          bool
	KbIDs           []int64
	TopK            int32
	ScoreThreshold  float64
	HistoryTurns    int
	MaxContextChars int
}

// PlanFrom 由 Agent 定义与请求覆盖项合成运行计划。
//
// 覆盖优先级：请求显式值 > Agent 配置 > 全局默认。
// ★ 区分"未设置"与"设置为 0"：
//
//	use_rag 为 false 与未指定语义不同 —— 前者是明确关闭 RAG，
//	后者是"沿用 Agent 配置"。proto3 默认值都是零值，
//	只能靠请求中的 kb_ids 是否为空来辅助判断，
//	因此调用方应显式填充 use_rag。
func PlanFrom(agent *agentv1.Agent, req *agentv1.ChatReq, cfg conf.AgentConf, ragCfg conf.RAGConf) AgentPlan {
	plan := AgentPlan{
		SystemPrompt:    cfg.DefaultSystemPrompt,
		Model:           cfg.DefaultModel,
		Temperature:     float32(cfg.DefaultTemperature),
		MaxTokens:       int32(cfg.DefaultMaxTokens),
		TopK:            int32(ragCfg.DefaultTopK),
		ScoreThreshold:  ragCfg.DefaultScoreThreshold,
		HistoryTurns:    cfg.HistoryTurns,
		MaxContextChars: cfg.MaxContextChars,
	}

	if agent != nil {
		if agent.SystemPrompt != "" {
			plan.SystemPrompt = agent.SystemPrompt
		}
		if agent.Model != "" {
			plan.Model = agent.Model
		}
		if agent.Temperature != 0 {
			plan.Temperature = agent.Temperature
		}
		if agent.MaxTokens != 0 {
			plan.MaxTokens = agent.MaxTokens
		}
		if agent.RagTopK > 0 {
			plan.TopK = agent.RagTopK
		}
		if agent.RagScoreThreshold > 0 {
			plan.ScoreThreshold = agent.RagScoreThreshold
		}
		plan.Tools = agent.Tools
		plan.KbIDs = agent.KbIds
		plan.UseRAG = agent.RagEnabled
	}

	// 请求级覆盖
	if req.GetModel() != "" {
		plan.Model = req.GetModel()
	}
	if len(req.GetKbIds()) > 0 {
		plan.KbIDs = req.GetKbIds()
		// 显式指定知识库即视为要检索，即便 Agent 未开 RAG
		plan.UseRAG = true
	}
	if req.GetUseRag() {
		plan.UseRAG = true
	}
	if plan.TopK <= 0 {
		plan.TopK = int32(ragCfg.DefaultTopK)
	}
	return plan
}

// resolveKBMounts 当 plan 未显式指定 kb_ids 时，
// 尝试从 rag_service 的绑定表补全该 agent 的知识库。
//
// ★ 绑定到 KB 即视为要检索（与请求显式传 kb_ids 的语义一致）：
//
//	用户在 KB 管理页把知识库挂到 agent 下，期望的正是对话自动带上检索。
//	因此不要求 plan.UseRAG 前置 —— 有绑定就强制开启；
//	无绑定时维持 plan.UseRAG 原值（可能来自 Agent 的显式开关）。
func (o *Orchestrator) resolveKBMounts(ctx context.Context, agentID int64, plan *AgentPlan) {
	// 请求/Agent 已显式指定 kb_ids：尊重它，不重复拉取
	if len(plan.KbIDs) > 0 || o.kbMount == nil {
		return
	}
	ids, err := o.kbMount.ListAgentKB(ctx, agentID)
	if err != nil {
		logger.Warn(ctx, "拉取 agent 知识库绑定失败，本次对话不检索",
			"agent_id", agentID, "err", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	plan.KbIDs = ids
	plan.UseRAG = true
}

// RetrievalContext 检索结果（上下文 + 引用映射）
type RetrievalContext struct {
	// ContextText 注入给模型的资料正文
	ContextText string
	// Citations 引用列表，顺序即模型看到的编号顺序
	Citations []*agentv1.Citation
	// Degraded 是否走了降级
	Degraded bool
	// Reason 降级原因
	Reason string
	// ElapsedMS 检索耗时
	ElapsedMS int64
	// HitCount 命中条数
	HitCount int
}

// Retrieve 执行 RAG 检索。
//
// ★ 本方法永不返回 error：检索失败对用户而言应表现为
//
//	"没有资料可用"而非"系统错误" —— 对话仍可继续。
//	失败信息通过 Degraded/Reason 透传到响应，
//	让前端能如实提示"未能检索到资料"。
func (o *Orchestrator) Retrieve(ctx context.Context, query string, plan AgentPlan, id *commonv1.Identity, reqID string) RetrievalContext {
	start := time.Now()
	out := RetrievalContext{}

	if !plan.UseRAG || len(plan.KbIDs) == 0 || o.rag == nil {
		return out
	}
	if strings.TrimSpace(query) == "" {
		return out
	}

	resp, err := o.rag.Retrieve(ctx, &ragv1.RetrieveReq{
		Query:          query,
		KbIds:          plan.KbIDs,
		TopK:           plan.TopK,
		ScoreThreshold: plan.ScoreThreshold,
		ContextWindow:  int32(o.opt.RAGCfg.ContextWindow),
		Identity:       id,
		RequestId:      reqID,
	})
	out.ElapsedMS = time.Since(start).Milliseconds()

	if err != nil {
		out.Degraded = true
		out.Reason = "检索失败: " + err.Error()
		logger.Warn(ctx, "rag retrieve failed, 降级为纯 LLM 回答",
			"err", err, "kb_ids", plan.KbIDs)
		return out
	}

	out.Degraded = resp.GetDegraded()
	out.Reason = resp.GetDegradeReason()
	out.HitCount = len(resp.GetHits())

	// 逐条建引用，编号从 1 开始 —— 与模型回答里的 [n] 严格对应
	var sb strings.Builder
	budget := plan.MaxContextChars
	if budget <= 0 {
		budget = 4000
	}
	used := 0

	for _, h := range resp.GetHits() {
		content := h.GetContent()
		if strings.TrimSpace(content) == "" {
			continue
		}
		// ★ 预算控制：超长切片直接截断。
		//   检索可能命中整篇长文，一条就能吃掉整个上下文窗口，
		//   导致真正的提问内容被挤掉 —— 这是 RAG 最常见的失效方式。
		snippet := content
		remain := budget - used
		if remain <= 0 {
			break
		}
		if len(snippet) > remain {
			snippet = snippet[:remain] + "…"
		}
		used += len(snippet)

		idx := int32(len(out.Citations) + 1)
		out.Citations = append(out.Citations, &agentv1.Citation{
			Index:    idx,
			KbId:     h.GetKbId(),
			DocId:    h.GetDocId(),
			ChunkId:  h.GetChunkId(),
			DocTitle: h.GetDocTitle(),
			PageNo:   h.GetPageNo(),
			Score:    h.GetScore(),
			Snippet:  truncate(content, 200),
		})

		fmt.Fprintf(&sb, "[%d] 来自《%s》", idx, h.GetDocTitle())
		if h.GetPageNo() > 0 {
			fmt.Fprintf(&sb, " 第%d页", h.GetPageNo())
		}
		fmt.Fprintf(&sb, "（相关度 %.2f）：\n%s\n\n", h.GetScore(), snippet)
	}

	out.ContextText = strings.TrimRight(sb.String(), "\n")
	return out
}

// BuildMessages 拼装发给模型的完整消息列表。
//
// 顺序：system 提示词 → 历史（含 tool 角色）→ 本轮用户提问。
// RAG 资料作为 system 的一部分注入（而非塞进 user 消息），
// 这样模型不会把它当成用户的话去回应。
//
// ★ 现在会把 tool 消息也带上：ReAct 循环里 assistant 返回 tool_call
//
//	后，tool 执行结果要作为 role="tool" 的消息喂回 LLM。
func (o *Orchestrator) BuildMessages(plan AgentPlan, query string, rc RetrievalContext, history []*agentv1.ChatMessage) []*llmv1.Message {
	var msgs []*llmv1.Message

	system := plan.SystemPrompt
	if rc.ContextText != "" {
		system = strings.TrimSpace(system) + "\n\n" +
			"以下是检索到的参考资料，请优先基于这些资料回答，并在句末用 [编号] 标注引用来源：\n\n" +
			rc.ContextText
	}
	msgs = append(msgs, &llmv1.Message{Role: "system", Content: system})

	// 历史：按时间正序，包含 user / assistant / tool 三种角色
	for _, m := range history {
		if m.GetContent() == "" && m.GetRole() != "tool" {
			continue
		}
		role := m.GetRole()
		if role == "" {
			role = "user"
		}
		pb := &llmv1.Message{Role: role, Content: m.GetContent()}
		if role == "tool" {
			// agentv1.ChatMessage 没有 Name 字段，工具名由 ReAct 循环
			// 写进 content 前缀 "[tool:xxx]\n..."，回放时解析出来。
			// ★ 不能直接传 m.GetRole()（那是 "tool"，不是工具名）。
			pb.Name = toolNameFromContent(m.GetContent())
			// ★ tool 消息的 tool_call_id 从 ChatMessage 结构里没有直接字段，
			//   由 ReAct 循环里构造时确保每条 tool 消息都带对应的 tool_call_id。
			//   这里历史回放时如果缺失，LLM 可能报错；保守做法是不带。
		}
		msgs = append(msgs, pb)
	}

	msgs = append(msgs, &llmv1.Message{Role: "user", Content: query})
	return msgs
}

// Result 一次对话的结果
type Result struct {
	Message    *agentv1.ChatMessage
	Degraded   *commonv1.Degradation
	RetrieveMS int64
	LLMMS      int64
	TotalMS    int64
}

// Chat 同步对话 —— 带 ReAct 工具执行循环。
//
// 流程：
//  1. RAG 检索（tool_call 轮也要带，让 LLM 知道上下文）
//  2. 循环（最多 maxRnds 轮）：
//     a. 调 LLM（messages 里累积 assistant/tool 历史）
//     b. finish_reason=stop → 跳出循环，用本轮 content 作为最终回答
//     c. finish_reason=tool_calls → 逐个执行工具，把 assistant+tool 消息落库并 append 进 messages，继续循环
//     d. 超限 → 用当前 content（可能是空）作为截断回答
func (o *Orchestrator) Chat(ctx context.Context, agent *agentv1.Agent, req *agentv1.ChatReq, sessionID int64, id *commonv1.Identity) (*Result, error) {
	start := time.Now()
	query := req.GetContent()
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("content 不能为空")
	}

	plan := PlanFrom(agent, req, o.opt.Agent, o.opt.RAGCfg)
	o.resolveKBMounts(ctx, agent.GetAgentId(), &plan)

	// 0) 检索（RAG 上下文进 system prompt，不参与 ReAct messages）
	rc := o.Retrieve(ctx, query, plan, id, req.GetRequestId())

	// 1) 初始 messages：system + 历史 + 本轮 user
	history := o.store.RecentMessages(ctx, sessionID, plan.HistoryTurns*2)
	msgs := o.BuildMessages(plan, query, rc, history)

	// 2) ReAct 循环
	var finalContent string
	var finalUsage *agentv1.Usage
	var totalLLMMS int64
	var degraded *commonv1.Degradation

	for round := 0; round < o.maxRnds; round++ {
		llmStart := time.Now()
		resp, err := o.llm.Chat(ctx, &llmv1.ChatReq{
			Model:       plan.Model,
			Messages:    msgs,
			Identity:    id,
			Temperature: plan.Temperature,
			MaxTokens:   plan.MaxTokens,
			Tools:       plan.Tools,
			RequestId:   req.GetRequestId(),
		})
		thisMS := time.Since(llmStart).Milliseconds()
		totalLLMMS += thisMS
		if err != nil {
			// LLM 直接失败（超时/网络），降级返回
			return nil, fmt.Errorf("调用模型失败: %w", err)
		}

		// 把 assistant 消息加进 messages（不管有没有 tool_call）
		respMsgs := messagesFromLLM(resp)
		msgs = append(msgs, respMsgs...)

		// 本轮无 tool_call → 结束循环
		if len(resp.GetToolCalls()) == 0 {
			finalContent = resp.GetContent()
			finalUsage = &agentv1.Usage{
				PromptTokens:     resp.GetUsage().GetPromptTokens(),
				CompletionTokens: resp.GetUsage().GetCompletionTokens(),
				TotalTokens:      resp.GetUsage().GetTotalTokens(),
			}
			if resp.GetDegradation() != nil {
				degraded = resp.GetDegradation()
			}
			break
		}

		// 本轮有 tool_call —— 执行每个工具，把 tool 结果加进 messages
		if o.tool == nil {
			logger.Warn(ctx, "LLM 返回 tool_call 但 ToolExec 未注入，跳过工具执行", "tool_calls", len(resp.GetToolCalls()))
			finalContent = resp.GetContent() + "\n\n（工具执行未启用，已跳过。）"
			finalUsage = &agentv1.Usage{
				PromptTokens:     resp.GetUsage().GetPromptTokens(),
				CompletionTokens: resp.GetUsage().GetCompletionTokens(),
				TotalTokens:      resp.GetUsage().GetTotalTokens(),
			}
			break
		}

		// 先落库 assistant 消息（含 tool_calls 标记——content 可能为空）
		assistantSaved, err := o.store.AppendMessage(ctx, sessionID, &agentv1.ChatMessage{
			Role:          "assistant",
			Content:       resp.GetContent(), // tool_call 轮通常 content 是空字符串
			Degraded:      resp.GetDegradation().GetDegraded(),
			DegradeReason: resp.GetDegradation().GetReason(),
		})
		if err != nil {
			logger.Error(ctx, "保存 assistant(tool_call) 消息失败", "session_id", sessionID, "err", err)
		}
		_ = assistantSaved // 暂不返回给用户

		// 执行每个 tool_call
		for _, tc := range resp.GetToolCalls() {
			result, execErr := o.tool.Execute(ctx, tc.GetName(), tc.GetArguments())
			if execErr != nil {
				result = fmt.Sprintf(`{"error":%q}`, execErr.Error())
				logger.Warn(ctx, "工具执行失败", "tool", tc.GetName(), "err", execErr)
			}

			// 把 tool 结果加进 messages（带 tool_call_id）
			msgs = append(msgs, &llmv1.Message{
				Role:       "tool",
				Content:    result,
				Name:       tc.GetName(),
				ToolCallId: tc.GetId(),
			})

			// 落库 tool 消息（agentv1.ChatMessage 无 Name 字段，工具名进 content 前缀）
			storedContent := fmt.Sprintf("[tool:%s]\n%s", tc.GetName(), result)
			if _, err := o.store.AppendMessage(ctx, sessionID, &agentv1.ChatMessage{
				Role:    "tool",
				Content: storedContent,
			}); err != nil {
				logger.Error(ctx, "保存 tool 消息失败", "session_id", sessionID, "err", err)
			}
		}

		// 继续下一轮循环——messages 里已有 assistant(tool_call) + tool(result)
		// 下一轮 LLM 会看到完整的 ReAct 上下文
	}

	// 循环结束后 finalContent 可能还是空（LLM 每轮都只发 tool_call），
	// 但这是模型决策，不应阻塞整条链路
	if finalContent == "" {
		finalContent = "（模型返回工具调用但未能生成最终回答，已尝试 " + fmt.Sprintf("%d", o.maxRnds) + " 轮）"
	}

	// 最终落库 assistant 消息
	msg := &agentv1.ChatMessage{
		Role:          "assistant",
		Content:       finalContent,
		Citations:     rc.Citations,
		Degraded:      degraded.GetDegraded() || rc.Degraded,
		DegradeReason: firstNonEmpty(degraded.GetReason(), rc.Reason),
		Usage:         finalUsage,
	}
	saved, err := o.store.AppendMessage(ctx, sessionID, msg)
	if err != nil {
		logger.Error(ctx, "保存助手消息失败（回答仍返回）", "session_id", sessionID, "err", err)
		saved = msg
	}

	return &Result{
		Message:    saved,
		Degraded:   toDegradation(msg),
		RetrieveMS: rc.ElapsedMS,
		LLMMS:      totalLLMMS,
		TotalMS:    time.Since(start).Milliseconds(),
	}, nil
}

// messagesFromLLM 把 ChatResp 里的 tool_calls 转成可回喂给 LLM 的 Message 数组。
// LLM 返回 tool_call 时，assistant 消息需要同时带 tool_calls 数组（ID 关联后续 tool 消息）。
func messagesFromLLM(resp *llmv1.ChatResp) []*llmv1.Message {
	if len(resp.GetToolCalls()) == 0 {
		return []*llmv1.Message{{Role: "assistant", Content: resp.GetContent()}}
	}
	m := &llmv1.Message{Role: "assistant", Content: resp.GetContent()}
	for _, tc := range resp.GetToolCalls() {
		m.ToolCalls = append(m.ToolCalls, &llmv1.ToolCall{
			Id:        tc.GetId(),
			Name:      tc.GetName(),
			Arguments: tc.GetArguments(),
		})
	}
	return []*llmv1.Message{m}
}

// messagesFromStream 流式版 messagesFromLLM：把一轮流式输出的
// 自然语言增量 + 收集到的 tool_calls 转成 assistant 消息。
// 与同步版行为一致：有 tool_call 时 assistant 消息带 tool_calls 数组，
// 供下一轮 ReAct 与跨会话历史回放关联 tool 消息。
func messagesFromStream(content string, toolCalls []*llmv1.ToolCall) []*llmv1.Message {
	if len(toolCalls) == 0 {
		return []*llmv1.Message{{Role: "assistant", Content: content}}
	}
	m := &llmv1.Message{Role: "assistant", Content: content}
	for _, tc := range toolCalls {
		m.ToolCalls = append(m.ToolCalls, &llmv1.ToolCall{
			Id:        tc.GetId(),
			Name:      tc.GetName(),
			Arguments: tc.GetArguments(),
		})
	}
	return []*llmv1.Message{m}
}

// StreamEvent 流式事件（已转换为 agent 契约的 ChatEvent）
type StreamEvent struct {
	Type  string
	Delta string
	// Reasoning 思维链增量（推理模型输出）
	Reasoning  string
	SessionID  int64
	MsgID      int64
	HitCount   int32
	RetrieveMS int64
	LLMMS      int64
	Message    *agentv1.ChatMessage
	Citation   *agentv1.Citation
	Usage      *agentv1.Usage
	Degraded   *commonv1.Degradation
	ErrorCode  string
	ErrorMsg   string
}

// 事件类型常量（与 proto ChatEvent.type 对齐）
const (
	EventSession   = "session"
	EventRetrieval = "retrieval"
	EventDelta     = "delta"
	EventReasoning = "reasoning"
	EventCitation  = "citation"
	EventDone      = "done"
	EventError     = "error"
)

// ChatStream 流式对话 —— 带 ReAct 工具执行循环。
//
// 事件顺序：session → retrieval → citation* → delta* → done
//
// 与同步 Chat 的 ReAct 循环对齐：
//
//	for round := 0; round < maxRnds; round++ {
//	    流式调 LLM（带 tools）
//	    收集 delta（emit 给前端）+ tool_calls
//	    无 tool_call → 结束
//	    有 tool_call → 执行工具，tool 结果喂回，继续下一轮
//	}
//
// 多轮产生的自然语言增量（工具调用前后 LLM 说的话）连续 emit，
// 前端感知为一条不间断的回答；落库时累积为最终 assistant 消息。
func (o *Orchestrator) ChatStream(ctx context.Context, agent *agentv1.Agent, req *agentv1.ChatReq, sessionID int64, id *commonv1.Identity, emit func(*StreamEvent) bool) error {
	query := req.GetContent()
	if strings.TrimSpace(query) == "" {
		return errors.New("content 不能为空")
	}

	plan := PlanFrom(agent, req, o.opt.Agent, o.opt.RAGCfg)
	o.resolveKBMounts(ctx, agent.GetAgentId(), &plan)

	// 先落库用户消息 —— 保证上下文完整，
	// 也让会话列表立刻能看到这条提问。
	if _, err := o.store.AppendMessage(ctx, sessionID, &agentv1.ChatMessage{
		Role:    "user",
		Content: query,
	}); err != nil {
		logger.Error(ctx, "保存用户消息失败", "session_id", sessionID, "err", err)
	}

	if !emit(&StreamEvent{Type: EventSession, SessionID: sessionID}) {
		return nil
	}

	// 1) 检索
	rc := o.Retrieve(ctx, query, plan, id, req.GetRequestId())
	if !emit(&StreamEvent{
		Type:       EventRetrieval,
		HitCount:   int32(rc.HitCount),
		RetrieveMS: rc.ElapsedMS,
	}) {
		return nil
	}
	for _, c := range rc.Citations {
		if !emit(&StreamEvent{Type: EventCitation, Citation: c}) {
			return nil
		}
	}

	// 2) 初始上下文（历史 + 本轮提问）
	history := o.store.RecentMessages(ctx, sessionID, plan.HistoryTurns*2)
	msgs := o.BuildMessages(plan, query, rc, history)

	// 3) ReAct 流式循环
	var (
		totalLLMMS int64
		finalUsage *llmv1.Usage
		degraded   *commonv1.Degradation
		answer     strings.Builder // 多轮自然语言增量累积（含工具调用前后的叙述）
		completed  bool
	)

	for round := 0; round < o.maxRnds; round++ {
		llmStart := time.Now()
		stream, err := o.llm.ChatStream(ctx, &llmv1.ChatReq{
			Model:       plan.Model,
			Messages:    msgs,
			Identity:    id,
			Temperature: plan.Temperature,
			MaxTokens:   plan.MaxTokens,
			Tools:       plan.Tools,
			Stream:      true,
			RequestId:   req.GetRequestId(),
		})
		if err != nil {
			return fmt.Errorf("调用模型失败: %w", err)
		}

		roundText, _, usage, toolCalls, ldeg, llmMS, broken, aborted := o.consumeStream(ctx, stream, emit, llmStart)
		totalLLMMS += llmMS
		if broken || aborted {
			return nil
		}

		// 本轮自然语言增量进最终回答（delta 已 emit 给前端）
		answer.WriteString(roundText)
		// assistant 消息（含 tool_calls）进 messages，供下一轮与历史回放
		msgs = append(msgs, messagesFromStream(roundText, toolCalls)...)

		// 本轮无 tool_call → ReAct 结束
		if len(toolCalls) == 0 {
			finalUsage = usage
			degraded = ldeg
			completed = true
			break
		}

		// 有 tool_call 但执行器未注入：跳过并如实告知（降级为纯对话）
		if o.tool == nil {
			logger.Warn(ctx, "LLM 返回 tool_call 但 ToolExec 未注入，跳过工具执行", "tool_calls", len(toolCalls))
			note := "\n\n（工具执行未启用，已跳过。）"
			answer.WriteString(note)
			if !emit(&StreamEvent{Type: EventDelta, Delta: note, LLMMS: totalLLMMS}) {
				return nil
			}
			finalUsage = usage
			degraded = ldeg
			completed = true
			break
		}

		// 落库 assistant(tool_call) 消息（tool_call 轮 content 通常为空）
		if _, err := o.store.AppendMessage(ctx, sessionID, &agentv1.ChatMessage{
			Role:          "assistant",
			Content:       roundText,
			Degraded:      ldeg.GetDegraded(),
			DegradeReason: ldeg.GetReason(),
		}); err != nil {
			logger.Error(ctx, "保存 assistant(tool_call) 消息失败", "session_id", sessionID, "err", err)
		}

		// 执行每个 tool_call，结果喂回 messages
		for _, tc := range toolCalls {
			result, execErr := o.tool.Execute(ctx, tc.GetName(), tc.GetArguments())
			if execErr != nil {
				result = fmt.Sprintf(`{"error":%q}`, execErr.Error())
				logger.Warn(ctx, "工具执行失败", "tool", tc.GetName(), "err", execErr)
			}

			msgs = append(msgs, &llmv1.Message{
				Role:       "tool",
				Content:    result,
				Name:       tc.GetName(),
				ToolCallId: tc.GetId(),
			})

			storedContent := fmt.Sprintf("[tool:%s]\n%s", tc.GetName(), result)
			if _, err := o.store.AppendMessage(ctx, sessionID, &agentv1.ChatMessage{
				Role:    "tool",
				Content: storedContent,
			}); err != nil {
				logger.Error(ctx, "保存 tool 消息失败", "session_id", sessionID, "err", err)
			}
		}
		// 下一轮：messages 已有 assistant(tool_call) + tool(result)，
		// LLM 会看到完整 ReAct 上下文
	}

	// 循环耗尽仍未出最终回答（LLM 每轮都只发 tool_call）
	if !completed {
		note := "（模型持续返回工具调用但未能生成最终回答）"
		answer.WriteString(note)
		emit(&StreamEvent{Type: EventDelta, Delta: note, LLMMS: totalLLMMS})
	}

	// 4) 落库并收尾
	msg := &agentv1.ChatMessage{
		Role:          "assistant",
		Content:       answer.String(),
		Citations:     rc.Citations,
		Degraded:      degraded.GetDegraded() || rc.Degraded,
		DegradeReason: firstNonEmpty(degraded.GetReason(), rc.Reason),
	}
	if finalUsage != nil {
		msg.Usage = &agentv1.Usage{
			PromptTokens:     finalUsage.GetPromptTokens(),
			CompletionTokens: finalUsage.GetCompletionTokens(),
			TotalTokens:      finalUsage.GetTotalTokens(),
		}
	}
	saved, err := o.store.AppendMessage(ctx, sessionID, msg)
	if err != nil {
		logger.Error(ctx, "保存助手消息失败", "session_id", sessionID, "err", err)
		saved = msg
	}

	emit(&StreamEvent{
		Type:      EventDone,
		MsgID:     saved.MsgId,
		Message:   saved,
		Usage:     saved.GetUsage(),
		Degraded:  toDegradation(saved),
		LLMMS:     totalLLMMS,
		SessionID: sessionID,
	})
	return nil
}

// consumeStream 持续从 llm 流式 endpoint 读取 chunk，向 emit 派发 delta / reasoning 事件并累计内容。
// 返回累积文本、reasoning、usage、toolCalls、degradation，以及最终 llmMS。
// broken=true 表示流式读取过程中遇到错误（已 emit EventError），
// aborted=true 表示 emit 返回 false（消费方断开），ChatStream 两种情况都应直接 return。
func (o *Orchestrator) consumeStream(
	ctx context.Context,
	stream llmservice.LLMService_ChatStreamClient,
	emit func(*StreamEvent) bool,
	llmStart time.Time,
) (
	content, reasoning string,
	usage *llmv1.Usage,
	toolCalls []*llmv1.ToolCall,
	ldeg *commonv1.Degradation,
	llmMS int64,
	broken, aborted bool,
) {
	var sb, sbReasoning strings.Builder
	llmMS = time.Since(llmStart).Milliseconds()

	for {
		chunk, err := stream.Recv()
		if err != nil {
			// io.EOF 属正常收尾，不是错误
			if errors.Is(err, io.EOF) {
				break // 正常收尾
			}
			logger.Error(ctx, "模型流式读取中断", "err", err)
			emit(&StreamEvent{Type: EventError, ErrorCode: "STREAM_BROKEN", ErrorMsg: err.Error(), LLMMS: llmMS})
			broken = true
			return
		}
		llmMS = time.Since(llmStart).Milliseconds()

		if d := chunk.GetDelta(); d != "" {
			sb.WriteString(d)
			if !emit(&StreamEvent{Type: EventDelta, Delta: d, LLMMS: llmMS}) {
				// 消费方已断开
				aborted = true
				return
			}
		}
		if rd := chunk.GetReasoningDelta(); rd != "" {
			sbReasoning.WriteString(rd)
			if !emit(&StreamEvent{Type: EventReasoning, Reasoning: rd, LLMMS: llmMS}) {
				aborted = true
				return
			}
		}
		if u := chunk.GetUsage(); u != nil {
			usage = u
		}
		if tc := chunk.GetToolCallDelta(); tc != nil {
			toolCalls = append(toolCalls, tc)
		}
		if d := chunk.GetDegradation(); d != nil {
			ldeg = d
		}
		if chunk.GetDone() {
			break
		}
	}
	content = sb.String()
	reasoning = sbReasoning.String()
	return
}

// ---------- 辅助 ----------

// toDegradation 把消息上的降级标记转成 proto 结构
func toDegradation(m *agentv1.ChatMessage) *commonv1.Degradation {
	if m == nil || !m.GetDegraded() {
		return nil
	}
	reason := m.GetDegradeReason()
	if reason == "" {
		reason = "未说明原因"
	}
	return &commonv1.Degradation{Degraded: true, Reason: reason}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// toolNameFromContent 从 tool 消息的 content 前缀提取工具名。
// 落库格式为 "[tool:<name>]\n<result>"（agentv1.ChatMessage 无 Name 字段，
// 工具名只能靠前缀携带）；解析失败返回空串，回放时保守不带 name。
func toolNameFromContent(s string) string {
	const prefix = "[tool:"
	if strings.HasPrefix(s, prefix) {
		if i := strings.IndexByte(s, ']'); i > len(prefix) {
			return s[len(prefix):i]
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
