// Package provider 定义大模型 Provider 的稳定接口。
//
// 这是 llm_service 的核心抽象（对应 ADR-003）。设计目标：
//
//  1. 业务层（Chat/Embed handler）只依赖本接口，不依赖任何厂商 SDK
//  2. einoProvider 与 rawHTTPProvider 可互换，通过配置切换
//  3. 能力差异用 Capabilities() 显式声明，而不是运行时报错
//
// 为什么不用 eino-ext 全家桶直接铺开：
//
//	eino 的模型抽象迭代快、破坏性变更多；把它隔离在 provider 实现内部，
//	升级 eino 时业务代码零改动。
package provider

import (
	"context"

	"github.com/aegis/pkg/errno"
)

// ---------- 领域模型（与 IDL 解耦）----------
//
// 为什么内部模型不复用 llmv1 的 pb 类型：
//  1. provider 是"厂商侧"抽象，不该被 IDL 变更牵连
//  2. 内部模型可携带 pb 表达不了的字段（原始响应、缓存命中）
//  3. 便于在没有 Kitex 依赖的环境下单独测试 provider

// Role 消息角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 单条消息
type Message struct {
	Role       Role
	Content    string
	Name       string
	ToolCallID string
}

// ToolCall 工具调用
type ToolCall struct {
	ID   string
	Name string
	// Arguments 为 JSON 字符串
	Arguments string
}

// Tool 工具定义
type Tool struct {
	Name        string
	Description string
	// Parameters 为 JSON Schema
	Parameters string
}

// ChatRequest 对话请求
type ChatRequest struct {
	Model           string
	Messages        []Message
	Temperature     float64
	MaxTokens       int
	Tools           []Tool
	EnableReasoning bool
	RequestID       string
	UID             int64
	Tenant          string
	InputType       string
}

// ChatResponse 对话响应
type ChatResponse struct {
	Content       string
	Reasoning     string
	ToolCalls     []ToolCall
	Model         string
	FinishReason  string
	Usage         Usage
	Provider      string
	Degraded      bool
	DegradeReason string
}

// EmbedRequest 向量化请求
type EmbedRequest struct {
	Model  string
	Texts  []string
	UID    int64
	Tenant string
	// InputType query / document，部分模型需区分
	InputType string
	// EnableSparse 是否同时生成稀疏向量（词项权重），用于 dense+sparse 混合检索
	EnableSparse bool
}

// SparseToken 稀疏向量的一维：{词表位置 → 权重}。
type SparseToken struct {
	Index int64
	Value float32
}

// RerankRequest 重排请求
type RerankRequest struct {
	Model string
	Query string
	// Documents 待重排的候选（来自向量检索的 topK 结果）
	Documents []string
	// TopN 返回前 N 条；<=0 表示全部返回
	TopN int
	// UID/Tenant 供计费与审计
	UID    int64
	Tenant string
	// ReturnDocuments 为 true 时在结果里回带原文，
	// 避免调用方再自己按 Index 去查切片表。
	ReturnDocuments bool
}

// RerankResult 单条重排结果
type RerankResult struct {
	// Index 在请求 Documents 中的下标（不是重排后的名次）
	Index int
	// Document 原文，仅当 RerankRequest.ReturnDocuments 为 true 时填充
	Document string
	// RelevanceScore 相关性分数。
	//
	// ★ 注意跨模型不可比：不同厂商的分数区间完全不同
	//   （实测百炼 qwen3.7-text-rerank 为 0–1，越大越相关；
	//   而 Cohere 的 rerank 分数区间与量纲都不同）。
	//   阈值必须按模型单独标定，不能沿用向量相似度的阈值。
	RelevanceScore float64
}

// RerankResponse 重排响应
type RerankResponse struct {
	// Results 按 RelevanceScore 降序
	Results []RerankResult
	Model   string
	Usage   Usage
}

// EmbedResponse 向量化响应
type EmbedResponse struct {
	Embeddings [][]float32
	// SparseEmbeddings 与 Embeddings 一一对应；仅 EnableSparse 且模型支持时非空
	SparseEmbeddings [][]SparseToken
	Model            string
	Dimension        int
	Usage            Usage
	// Cached[i] 表示第 i 个输入是否命中缓存
	Cached []bool
}

// Usage Token 计量
type Usage struct {
	PromptTokens     int32
	CompletionTokens int32
	TotalTokens      int32
	CachedTokens     int32
}

// ModelInfo 模型元信息
type ModelInfo struct {
	Name              string
	Provider          string
	BaseURL           string
	MaxTokens         int
	SupportsToolCall  bool
	SupportsVision    bool
	SupportsReasoning bool
	Status            string
	Priority          int
	InputPrice        float64
	OutputPrice       float64
}

// Capabilities Provider 能力声明
type Capabilities struct {
	ToolCall  bool
	Reasoning bool
	// Multimodal 视觉/多模态输入
	Multimodal bool
	Embeddings bool
	// Streaming 是否支持流式
	Streaming bool
}

// Provider 模型 Provider 稳定接口
type Provider interface {
	// Name 实现标识：eino / rawhttp / mock
	Name() string

	// Capabilities 显式声明能力，供上层做降级与路由决策
	Capabilities() Capabilities

	// Chat 同步对话
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)

	// ChatStream 流式对话。
	//
	// 返回的 channel 由 Provider 负责关闭；ctx 取消时应尽快停止生产。
	// 若 Provider 不支持流式（Capabilities().Streaming == false），
	// 实现可退化为一次性把完整结果推入 channel —— 但需在
	// ChatResponse.Degraded 中标注，由上层告知客户端。
	ChatStream(ctx context.Context, req *ChatRequest) (<-chan *Chunk, error)

	// Embed 向量化
	Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error)

	// Rerank 重排候选文档。
	//
	// ★ 为什么单独抽出而不是塞进 Embed：
	//   两者调用的是不同模型（embedding 模型 vs rerank 模型），
	//   不同端点、不同计费方式，且 rerank 不是所有部署都有 —
	//   没有就返回 ErrUnsupported，上层自动跳过这一环节，
	//   而不是让整条检索链路失败。
	//
	// 语义：把 Documents 按与 Query 的相关性重新排序，
	// 返回 topN 个（按相关性降序）。
	// 不支持时返回 ErrUnsupported。
	Rerank(ctx context.Context, req *RerankRequest) (*RerankResponse, error)

	// ListModels 列出可用模型
	ListModels(ctx context.Context) ([]ModelInfo, error)

	// Close 释放连接等资源
	Close() error
}

// Chunk 流式增量
type Chunk struct {
	Delta          string
	ReasoningDelta string
	ToolCallDelta  *ToolCall
	Done           bool
	Usage          Usage
	Degraded       bool
	DegradeReason  string
}

// ErrUnsupported 表示能力不支持。
//
// Provider 用它区分"能力缺失"与"调用失败"：
//   - 能力缺失 → 上层可切换到降级链上的下一个 Provider
//   - 调用失败 → 上层按重试/熔断处理
var ErrUnsupported = errno.New(errno.ErrNotImplemented)

// IsUnsupported 判断是否能力缺失
func IsUnsupported(err error) bool {
	return errno.CodeOf(err) == errno.ErrNotImplemented
}
