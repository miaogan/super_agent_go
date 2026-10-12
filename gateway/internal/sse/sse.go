// Package sse 把 agent_service 的流式事件转发为 SSE。
//
// 为什么用 Hertz 自带的 pkg/protocol/sse 而不是手写：
//
//	官方 Writer 已经处理了三件极易写错的事——
//	  - 每次写入后 Flush。不Flush 的话流式会退化成非流式，
//	    表现为"憋十几秒然后一次性全出来"，而代码看起来完全正确
//	  - data 含换行时按 SSE 规范拆成多行 data: 前缀
//	  - 结束时补写 0\r\n\r\n 收尾帧
//	自己造轮子几乎必然漏掉其中一条。
//
// 为什么 SSE 而不是 WebSocket：
//
//	对话是单向推送，SSE 足够；原生支持断线重连与 Last-Event-ID；
//	穿透网关/CDN/企业防火墙的兼容性远好于 WebSocket。
package sse

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	hzsse "github.com/cloudwego/hertz/pkg/protocol/sse"

	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	agentservice "github.com/aegis/idl/kitex_gen/agentv1/agentservice"
	"github.com/aegis/gateway/internal/middleware"
	"github.com/aegis/pkg/logger"
)

// AgentClient 调用 agent_service 的流式对话
type AgentClient interface {
	ChatStream(ctx context.Context, req *agentv1.ChatReq) (agentservice.AgentService_ChatStreamClient, error)
}

// Options 转发参数
type Options struct {
	Client AgentClient
	// Timeout 流式整体上限
	Timeout time.Duration
	// KeepAlive 心跳间隔；0 表示不发。
	//
	// ★ 心跳是必要的：经过 Nginx/LB 时空闲连接会被静默回收，
	//   表现为"思考一段时间后突然断开重连"。
	//   SSE 注释帧（":"）对客户端是透明的心跳。
	KeepAlive time.Duration
	// Retry 告知客户端断线重连间隔
	Retry time.Duration
}

// Forward 把 agent_service 的流式事件转发为 SSE。
func Forward(c *app.RequestContext, req *agentv1.ChatReq, opt Options) {
	ctx := ctxOf(c)

	// 响应头必须在任何写入之前设好
	// ★ 必须取指针：Response.Header 内含 nocopy 锁，按值赋值会拷贝锁状态
	h := &c.Response.Header
	h.Set(consts.HeaderContentType, "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set(consts.HeaderConnection, "keep-alive")
	// 明确禁止反向代理缓冲，否则 Nginx 会攒完才下发
	h.Set("X-Accel-Buffering", "no")

	w := hzsse.NewWriter(c)
	var eventID atomic.Int64

	// 先立即 Flush 响应头，让客户端知道连接已建立。
	// ★ 否则前端会一直等到首个 delta 才认为连接成功，
	//   而首次检索可能耗时数秒，表现为"卡住不动"。
	safeFlush(c)

	// 告知客户端断线后多久重连。
	// 走独立事件而非响应头：retry 必须是 SSE 协议的一部分，
	// HTTP 头对它无效。
	if opt.Retry > 0 {
		ev := hzsse.NewEvent()
		ev.SetRetry(opt.Retry)
		if err := w.Write(ev); err != nil {
			logger.Debug(ctx, "SSE retry 事件写入失败（客户端可能已断开）", "err", err)
		}
		safeFlush(c)
	}

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	streamCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stream, err := opt.Client.ChatStream(streamCtx, req)
	if err != nil {
		logger.Error(ctx, "调用 agent_service 流式对话失败", "err", err)
		writeJSON(c, w, &eventID, "error", map[string]any{
			"code": "UPSTREAM_UNAVAILABLE", "message": err.Error(),
		})
		w.Close()
		return
	}

	// 心跳协程：与读取并行，读取阻塞时仍能保活
	if opt.KeepAlive > 0 {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				// 心跳 panic 绝不能炸掉整个进程。
				// Hertz Writer 在连接被对端关闭等异常场景下内部可能 panic
				// （safeFlush 已有同类 recover，见同文件注释）。
				if r := recover(); r != nil {
					logger.Warn(ctx, "SSE 心跳 panic，心跳协程退出", "panic", r)
				}
			}()
			t := time.NewTicker(opt.KeepAlive)
			defer t.Stop()
			for {
				select {
				case <-streamCtx.Done():
					return
				case <-t.C:
					if err := w.WriteKeepAlive(); err != nil {
						// 心跳写失败 = 客户端已断开，主动取消让主循环尽快退出
						cancel()
						return
					}
				}
			}
		}()
		defer wg.Wait()
	}

	for {
		ev, err := stream.Recv()
		if err != nil {
			if isEOF(err) {
				break // 正常收尾
			}
			// ctx 取消：客户端断开或整体超时
			if streamCtx.Err() != nil {
				logger.Debug(ctx, "SSE 结束（客户端断开或超时）")
				break
			}
			logger.Warn(ctx, "SSE 流异常中断", "err", err)
			writeJSON(c, w, &eventID, "error", map[string]any{
				"code": "STREAM_BROKEN", "message": err.Error(),
			})
			break
		}

		if ev.GetType() == "error" {
			// 下游的 error 事件透传，让前端拿到明确原因
			writeJSON(c, w, &eventID, "error", map[string]any{
				"code":    ev.GetErrorCode(),
				"message": ev.GetErrorMessage(),
			})
			break
		}

		payload, err := marshalEvent(ev)
		if err != nil {
			logger.Warn(ctx, "序列化事件失败，跳过该帧", "type", ev.GetType(), "err", err)
			continue // 单帧序列化失败不应中断整体
		}

		id := strconv.FormatInt(eventID.Add(1), 10)
		if err := w.WriteEvent(id, ev.GetType(), []byte(payload)); err != nil {
			// ★ 写失败 = 客户端已断开。
			//   必须立即停止读取下游：否则会白白消耗模型 token，
			//   且没人消费的水流会让上游一直阻塞。
			logger.Debug(ctx, "SSE 客户端断开，停止读取下游")
			break
		}
	}

	w.Close()
}

// writeJSON 写入一个 JSON 数据事件
func writeJSON(c *app.RequestContext, w *hzsse.Writer, id *atomic.Int64, event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	if err := w.WriteEvent(strconv.FormatInt(id.Add(1), 10), event, data); err != nil {
		logger.Debug(context.Background(), "SSE writeJSON 写入失败（客户端可能已断开）", "err", err)
	}
	safeFlush(c)
}

// safeFlush 刷新响应缓冲，且不因 writer 异常而panic。
//
// ★ Hertz 的 Response.GetHijackWriter() 返回的是接口，
//
//	内部装的是 nil 指针时接口本身并不等于 nil，
//	所以 Flush() 内部解引用会 panic。
//	真实场景下（连接已被对端关闭、无网络栈的单元测试）
//	这都会发生，而一次 panic 会让整条 SSE 连接静默消失——
//	前端只看到"流到一半就没了"，连错误事件都收不到。
func safeFlush(c *app.RequestContext) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn(context.Background(), "SSE flush 失败", "panic", r)
		}
	}()
	_ = c.Flush()
}

func isEOF(err error) bool {
	return err == io.EOF || strings.Contains(err.Error(), "EOF")
}

// marshalEvent 把 pb 事件序列化为 JSON。
//
// ★ 手工构造 map 而非直接 json.Marshal(pb)：
//
//	pb 结构体含大量内部字段且 bytes 一律 base64，
//	直接序列化会产出又长又难读的 JSON，
//	前端拿不到想要的可读字段。
func marshalEvent(ev *agentv1.ChatEvent) (string, error) {
	out := map[string]any{"type": ev.GetType()}

	switch ev.GetType() {
	case "session":
		out["session_id"] = ev.GetSessionId()

	case "retrieval":
		out["hit_count"] = ev.GetHitCount()
		out["retrieve_ms"] = ev.GetRetrieveMs()

	case "delta", "reasoning":
		out["delta"] = ev.GetDelta()

	case "citation":
		if cc := ev.GetCitation(); cc != nil {
			out["citation"] = map[string]any{
				"index":     cc.GetIndex(),
				"kb_id":     cc.GetKbId(),
				"doc_id":    cc.GetDocId(),
				"chunk_id":  cc.GetChunkId(),
				"doc_title": cc.GetDocTitle(),
				"page_no":   cc.GetPageNo(),
				"score":     cc.GetScore(),
				"snippet":   cc.GetSnippet(),
			}
		}

	case "done":
		out["msg_id"] = ev.GetMsgId()
		if m := ev.GetMessage(); m != nil {
			out["session_id"] = m.GetSessionId()
			out["content"] = m.GetContent()
			out["degraded"] = m.GetDegraded()
			if r := m.GetDegradeReason(); r != "" {
				out["degrade_reason"] = r
			}
			out["citations"] = marshalCitations(m.GetCitations())
		}
		if u := ev.GetUsage(); u != nil {
			out["usage"] = map[string]any{
				"prompt_tokens":     u.GetPromptTokens(),
				"completion_tokens": u.GetCompletionTokens(),
				"total_tokens":      u.GetTotalTokens(),
			}
		}
		if d := ev.GetDegradation(); d != nil {
			out["degradation"] = map[string]any{
				"degraded": d.GetDegraded(),
				"reason":   d.GetReason(),
			}
		}
		out["llm_ms"] = ev.GetLlmMs()

	case "error":
		out["code"] = ev.GetErrorCode()
		out["message"] = ev.GetErrorMessage()
	}

	b, err := json.Marshal(out)
	return string(b), err
}

func marshalCitations(cites []*agentv1.Citation) []map[string]any {
	out := make([]map[string]any, 0, len(cites))
	for _, cc := range cites {
		if cc == nil {
			continue
		}
		out = append(out, map[string]any{
			"index":     cc.GetIndex(),
			"kb_id":     cc.GetKbId(),
			"doc_id":    cc.GetDocId(),
			"chunk_id":  cc.GetChunkId(),
			"doc_title": cc.GetDocTitle(),
			"page_no":   cc.GetPageNo(),
			"score":     cc.GetScore(),
			"snippet":   cc.GetSnippet(),
		})
	}
	return out
}

// ctxOf 从 Hertz request context 中取出中间件注入的 context。
//
// 与 handler 包的 ctxOf 逻辑一致：优先复用中间件写入的 ctx（已含追踪 ID、
// 身份信息、上游超时），否则退化为 background。
func ctxOf(c *app.RequestContext) context.Context {
	if v, ok := c.Get(middleware.CtxKey); ok {
		if ctx, ok := v.(context.Context); ok && ctx != nil {
			return ctx
		}
	}
	return context.Background()
}
