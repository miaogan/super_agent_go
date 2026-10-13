// Package conf 统一配置结构。
//
// 设计要点：
//   - 所有服务共用同一份结构，各自只关心自己那段（结构体嵌入）
//   - 支持本地 YAML 与环境变量覆盖（本地开发无需 Nacos）
//
// ★ 尚未实现：从 Nacos 拉取配置与热更新。
//
//	历史上这里写的是"生产环境由 Nacos 下发（见 pkg/nacos）"，但 pkg/nacos
//	从未存在，配置来源实际只有「本地 YAML + 环境变量」两种。
//	NacosConf.Enabled 目前只影响服务注册路径，不影响配置加载。
//
// ★ mock 开关集中在此，是冒烟测试的关键：mock.enabled=true 时
//
//	各服务不依赖 Nacos / PostgreSQL / Milvus / 外部 LLM 也能启动。
package conf

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 全量配置
type Config struct {
	Service  ServiceConf  `yaml:"service"`
	Log      LogConf      `yaml:"log"`
	JSON     JSONConf     `yaml:"json"`
	Nacos    NacosConf    `yaml:"nacos"`
	Mock     MockConf     `yaml:"mock"`
	Server   ServerConf   `yaml:"server"`
	Postgres PostgresConf `yaml:"postgres"`
	Redis    RedisConf    `yaml:"redis"`
	S3       S3Conf       `yaml:"s3"`
	Minio    MinioConf    `yaml:"minio"`

	Auth   AuthConf   `yaml:"auth"`
	LLM    LLMConf    `yaml:"llm"`
	RAG    RAGConf    `yaml:"rag"`
	Milvus MilvusConf `yaml:"milvus"`

	Agent   AgentConf   `yaml:"agent"`
	Gateway GatewayConf `yaml:"gateway"`

	Upstream UpstreamConf `yaml:"upstream"`

	Internal InternalConf `yaml:"internal"`

	Metrics MetricsConf `yaml:"metrics"`
	OTel    OTelConf    `yaml:"otel"`
}

// ServiceConf 服务基本信息
type ServiceConf struct {
	Name    string `yaml:"name"`
	Port    int    `yaml:"port"`
	Version string `yaml:"version"`
}

// LogConf 日志配置
type LogConf struct {
	Level string `yaml:"level"` // debug / info / warn / error
	JSON  bool   `yaml:"json"`
}

// JSONConf JSON 引擎配置
//
// ★ 引擎选择是「构建期 + 启动自检」两段式的：
//
//	构建期：go build -tags sonic 决定编译哪套实现（见 pkg/jsonx）
//	运行期：本配置只决定「是否启用」与「不符预期时是否报错」
//
// 为什么不做成纯运行期开关？因为 Sonic 依赖 JIT，
// 未编译进去的代码不可能在运行期凭空启用 —— 那样的开关是假的。
type JSONConf struct {
	// Engine 选择引擎：auto / sonic / std
	//
	//   auto（默认）→ 用编译进来的那个，不做任何干预
	//   sonic        → 强制要求 Sonic；若构建时未启用则启动失败
	//   std          → 强制要求标准库；若构建时启用了 Sonic 则启动失败
	//
	// 「强制」的语义是宁可启动失败也不要静默降级：
	// 生产环境以为自己跑在 Sonic 上，实际跑在标准库，
	// 这种偏差往往在压测时才暴露成 inexplicable 的延迟毛刺。
	Engine string `yaml:"engine"`

	// Strict 为 true 时，Engine 与实际引擎不符时拒绝启动。
	//
	// 默认 false（仅打警告日志），便于本地开发与冒烟测试；
	// 生产建议置true，把配置错误挡在启动阶段。
	Strict bool `yaml:"strict"`

	// PrintEngine 为 true 时启动日志打印引擎信息（默认 true）。
	//
	// 关掉它没有性能收益 —— 只是一行日志 ——
	// 但会让「以为用了 Sonic 实际没有」变成不可排查的问题。
	PrintEngine bool `yaml:"print_engine"`
}

// JSONEngine 取值
const (
	JSONEngineAuto  = "auto"
	JSONEngineSonic = "sonic"
	JSONEngineStd   = "std"
)

// NacosConf 注册与配置中心
type NacosConf struct {
	Enabled     bool     `yaml:"enabled"`
	Addrs       []string `yaml:"addrs"`
	Namespace   string   `yaml:"namespace"`
	Group       string   `yaml:"group"`
	DataID      string   `yaml:"data_id"`
	Username    string   `yaml:"username"`
	Password    string   `yaml:"password"`
	LogLevel    string   `yaml:"log_level"`
	CacheDir    string   `yaml:"cache_dir"`
	DialTimeout string   `yaml:"dial_timeout"`

	// 服务注册
	RegisterEnabled bool   `yaml:"register_enabled"`
	Cluster         string `yaml:"cluster"`
	Weight          int    `yaml:"weight"`
}

// MockConf ★ 冒烟测试开关
type MockConf struct {
	// Enabled 为 true 时，各服务使用进程内 mock 实现：
	//   - 不连接 PostgreSQL / Milvus / Redis
	//   - 不连接 Nacos（若 nacos.enabled 也为 false）
	//   - LLM / RAG / Agent 返回可预测的构造数据
	Enabled bool `yaml:"enabled"`

	// LatencyMS 模拟耗时，用于验证流式与超时逻辑
	LatencyMS int `yaml:"latency_ms"`

	// FailRate 模拟失败率（0-100），用于验证降级链路
	FailRate int `yaml:"fail_rate"`

	// FailTargets 指定哪些下游被模拟为失败，如 ["milvus","llm"]
	FailTargets []string `yaml:"fail_targets"`
}

// IsFail 判断指定目标是否应模拟失败
func (m MockConf) IsFail(target string) bool {
	for _, t := range m.FailTargets {
		if strings.EqualFold(t, target) {
			return true
		}
	}
	return false
}

// ServerConf 服务监听
type ServerConf struct {
	// RPC 监听地址（Kitex）
	RPCAddr string `yaml:"rpc_addr"`
	// HTTP 监听地址（gateway 与各服务的健康检查端口）
	HTTPAddr string `yaml:"http_addr"`
	// 优雅退出等待时长
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// PostgresConf 关系型数据库
type PostgresConf struct {
	DSN             string        `yaml:"dsn"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	// 单条语句超时
	QueryTimeout time.Duration `yaml:"query_timeout"`
}

// RedisConf 缓存
type RedisConf struct {
	Addrs    []string `yaml:"addrs"`
	Password string   `yaml:"password"`
	DB       int      `yaml:"db"`
	PoolSize int      `yaml:"pool_size"`
}

// S3Conf 对象存储（原始文档文件）
type S3Conf struct {
	Endpoint  string `yaml:"endpoint"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
	UseSSL    bool   `yaml:"use_ssl"`
	// 预签名 URL 有效期
	PresignExpire time.Duration `yaml:"presign_expire"`
}

// AuthConf 认证配置
type AuthConf struct {
	// JWT
	JWTSecret     string `yaml:"jwt_secret"`      // HS256 用于本地开发
	JWTPrivateKey string `yaml:"jwt_private_key"` // RS256 PEM 路径（生产）
	JWTPublicKey  string `yaml:"jwt_public_key"`  // RS256 PEM 路径
	Issuer        string `yaml:"issuer"`
	// Audience 令牌受众。
	//
	// ★ 必须配置：JWVerify 会拒绝 aud 不含此值的令牌，
	//   防止为服务 A 签发的令牌被拿去访问服务 B。
	Audience        []string      `yaml:"audience"`
	AccessTokenTTL  time.Duration `yaml:"access_token_ttl"`
	RefreshTokenTTL time.Duration `yaml:"refresh_token_ttl"`
	// 连续失败多少次后要求验证码
	CaptchaAfterFailures int `yaml:"captcha_after_failures"`
	// 登录失败锁定
	MaxLoginFailures int           `yaml:"max_login_failures"`
	LockDuration     time.Duration `yaml:"lock_duration"`

	// OAuth2.0 / OIDC 身份提供方（可配多个）
	IdPs []IdPConf `yaml:"idps"`
}

// IdPConf 单个身份提供方
type IdPConf struct {
	Name         string   `yaml:"name"`     // 展示名
	Provider     string   `yaml:"provider"` // 协议实现：oidc / oauth2
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	AuthorizeURL string   `yaml:"authorize_url"`
	TokenURL     string   `yaml:"token_url"`
	UserInfoURL  string   `yaml:"user_info_url"`
	JWKSURL      string   `yaml:"jwks_url"`
	RedirectURI  string   `yaml:"redirect_uri"`
	Scopes       []string `yaml:"scopes"`
	Enabled      bool     `yaml:"enabled"`
	// 是否使用 PKCE
	UsePKCE bool `yaml:"use_pkce"`
	// 从 IdP 的用户信息中取哪个字段作为用户名
	UsernameField string `yaml:"username_field"`
}

// LLMConf 模型服务配置
type LLMConf struct {
	// Provider 选择实现：eino / rawhttp（见 ADR-003）
	Provider string `yaml:"provider"`
	// 默认模型
	DefaultModel string `yaml:"default_model"`
	// 默认 Embedding 模型
	DefaultEmbedModel string `yaml:"default_embed_model"`
	// 默认向量维度（bge-m3 = 1024）
	DefaultEmbedDim int `yaml:"default_embed_dim"`
	// 请求超时
	Timeout time.Duration `yaml:"timeout"`
	// FallbackDisabled 关闭对话模型按 priority 的降级重试。
	//
	// ★ 为什么是反向字段：模型级降级是 README 承诺的能力，默认应当**开启**。
	//   若写成 fallback_enabled，普通 bool 的零值是 false，任何不经过
	//   Default() 的构造路径（测试里直接 new LLMConf、或未来某处只填部分字段）
	//   都会静默变成"降级关闭"——这类问题在日志和返回上都看不出来。
	//   反向字段让零值等于启用，不需要记住再设一次。
	FallbackDisabled bool `yaml:"fallback_disabled"`
	// 单次请求最大 token
	MaxTokens int `yaml:"max_tokens"`

	// 模型列表（支持多厂商 + 降级链）
	Models []ModelConf `yaml:"models"`

	// 缓存
	CacheEnabled  bool          `yaml:"cache_enabled"`
	EmbedCacheTTL time.Duration `yaml:"embed_cache_ttl"`
}

// ModelConf 单个模型定义
type ModelConf struct {
	Name        string  `yaml:"name"`     // 逻辑名，如 deepseek-chat
	Provider    string  `yaml:"provider"` // 厂商标识
	BaseURL     string  `yaml:"base_url"`
	APIKey      string  `yaml:"api_key"`
	RealModel   string  `yaml:"real_model"` // 实际模型名（与逻辑名不同时填）
	MaxTokens   int     `yaml:"max_tokens"`
	Temperature float64 `yaml:"temperature"`
	Priority    int     `yaml:"priority"` // 降级链优先级，越小越优先
	Status      string  `yaml:"status"`   // enabled / disabled

	SupportsToolCall  bool `yaml:"supports_tool_call"`
	SupportsVision    bool `yaml:"supports_vision"`
	SupportsReasoning bool `yaml:"supports_reasoning"`

	InputPrice  float64 `yaml:"input_price"` // 每千 token
	OutputPrice float64 `yaml:"output_price"`

	// RerankPath 重排模型在 DashScope 原生 API 路径中的服务名，形如
	// "text-rerank/text-rerank"。
	//
	// ★ 为什么路径段不能直接用模型名：
	//
	//	DashScope 原生重排路径形如
	//	  /api/v1/services/rerank/{服务名}/{服务名}
	//	而请求体里的 model 字段填的是完整模型名（如 qwen3.7-text-rerank）。
	//	两处用的是不同的名字，很容易误以为「路径段 = 模型名」。
	//
	//	实测（2026-10-09，maas.qianwenaiapi.com，同一 Key 与请求体）：
	//	  text-rerank/text-rerank                 → 200
	//	  qwen3.7-text-rerank/text-rerank         → 400 url error
	//	  text-rerank/qwen3.7-text-rerank         → 400 url error
	//	  qwen3.7-text-rerank/qwen3.7-text-rerank → 400 url error
	//
	//	三种「看起来应该对」的写法全部 400，而错误信息只说
	//	"url error, please check url"，不会提示是路径段用错。
	//
	// 留空时 Rerank() 退化为不做重排（返回 ErrUnsupported），
	// 而不是拿模型名硬拼一个必然 400 的 URL 去浪费一次调用。
	RerankPath string `yaml:"rerank_path"`

	// EmbedNativePath 向量化模型在 DashScope 原生 API 路径中的服务名，
	// 形如 "text-embedding/text-embedding"。
	//
	// ★ 为什么需要它：OpenAI 兼容接口（/v1/embeddings）**不返回稀疏向量**
	//   （百炼文档明确：传 output_type=sparse 会被忽略，仅返回 200）。
	//   要拿 sparse_embedding 必须走 DashScope 原生接口
	//     /api/v1/services/embeddings/{服务名}/{服务名}
	//   该路径段同样是「服务名」而非模型名（与 RerankPath 同理）。
	//   留空时 Embed() 走 OpenAI 兼容接口（无稀疏向量）。
	EmbedNativePath string `yaml:"embed_native_path"`

	// EnableSparse 该模型是否支持生成稀疏向量（词项权重）。
	// 支持稀疏且配置了 EmbedNativePath 时，EnableSparse=true 的请求
	// 才会真正拿到 sparse_embedding。
	EnableSparse bool `yaml:"enable_sparse"`
}

// MinerUConf MinerU PDF 解析服务接入配置（见 pkg/parser/mineru.go）。
type MinerUConf struct {
	// BaseURL 服务根地址，不含 /file_parse。例：http://mineru.internal:8000
	BaseURL string `yaml:"base_url"`
	// Timeout 单次解析超时。版面分析很慢，默认 120s。
	Timeout time.Duration `yaml:"timeout"`
	// MaxResponseMB 响应体上限，默认 32MB。
	MaxResponseMB int `yaml:"max_response_mb"`
	// AuthToken 非空时以 Authorization: Bearer 发送。
	AuthToken string `yaml:"auth_token"`
}

// RAGConf 知识库与检索
type RAGConf struct {
	// 默认切片参数
	DefaultChunkSize    int `yaml:"default_chunk_size"`
	DefaultChunkOverlap int `yaml:"default_chunk_overlap"`
	// 默认召回数
	DefaultTopK int `yaml:"default_top_k"`
	// 默认相似度阈值
	DefaultScoreThreshold float64 `yaml:"default_score_threshold"`
	// MockScoreThreshold mock 模式专用阈值。
	//
	// ★ 为什么需要单独一项：
	//   生产阈值 0.3 是为真实 embedding（如 bge-m3）标定的 ——
	//   真实模型能给出 0.7+ 的强相关相似度。
	//   而 mock 用稀疏词袋模拟，其相似度上限受词袋模型数学限制：
	//   一个 8 token 的查询对 42 token 的切片，
	//   即便共享全部关键词，cos 也不可能超过 5/√(8×42) ≈ 0.24。
	//   强行套用 0.3 会让 mock 模式下检索恒为 0 命中，
	//   导致 RAG 链路无法被冒烟测试验证。
	//
	//   该值只在 mock 模式生效，不影响生产行为。
	MockScoreThreshold float64 `yaml:"mock_score_threshold"`

	// ---------- 重排（rerank）----------
	//
	// 检索链路：向量召回 topK → rerank 重排 → 取 topN → 拼 Prompt

	// RerankEnabled 是否启用重排。
	//
	// ★ 默认 true：重排是检索召回质量的关键环节，
	//   而它已被证明可以「配置缺失时自动降级」（见
	//   retrieve.Engine.rerank —— 未配置模型或调用失败时，
	//   退回按向量相似度排序并置 degraded 标记）。
	//
	//   也就是说：开着它最坏情况就是退化成不开的效果，
	//   关掉它则一定拿不到重排收益。故默认开启。
	//
	//   需要显式关闭的场景：向量召回量本身就小（比如 topK=3）
	//   而候选数远大于 topK，重排纯属浪费一次外部调用。
	RerankEnabled bool `yaml:"rerank_enabled"`

	// RerankModel 重排模型名（逻辑名，需在 llm.models 中存在）。
	// 留空则自动选用模型名含 "rerank" 的第一个启用模型。
	RerankModel string `yaml:"rerank_model"`

	// RerankTopK 送入重排的候选数。
	//
	// ★ 为什么需要比 DefaultTopK 大：
	//   rerank 的价值在于「把召回的前 K 条重新排序」，
	//   若 K 与最终取用的条数相同，就没有重排空间了。
	//   实测百炼 qwen3.7-text-rerank 单次可处理数十条，取 20 合理。
	RerankTopK int `yaml:"rerank_top_k"`

	// RerankScoreThreshold 重排后的相关性阈值，低于则丢弃。
	//
	// ★ 必须与 DefaultScoreThreshold 分开，因为量纲不同：
	//   DefaultScoreThreshold 是向量余弦相似度（生产实测 0.7+ 为强相关）
	//   RerankScoreThreshold 是 rerank 模型自己的分数
	//   （实测 qwen3.7-text-rerank 为 0–1，
	//     强相关 0.96、无关 0.003，区分度比向量更陡）
	//
	//   共用一个阈值是常见错误来源：拿0.3 去卡 rerank 分数会把
	//   大量中等相关的文档放进来，反而变差。
	RerankScoreThreshold float64 `yaml:"rerank_score_threshold"`

	// 上下文外扩窗口
	ContextWindow int `yaml:"context_window"`

	// 向量化并发
	IngestWorkers int `yaml:"ingest_workers"`

	// 单文件限制
	MaxFileSizeMB int `yaml:"max_file_size_mb"`
	MaxPages      int `yaml:"max_pages"`

	// 允许的文件类型
	AllowedTypes []string `yaml:"allowed_types"`

	// ---- PDF 解析引擎 ----
	//
	// PDFParser 取值：
	//   local  —— 内置纯 Go 文本抽取（pkg/parser）。**无 OCR**：
	//             扫描件 / 图片型 PDF 抽不出内容，会明确报解析失败。
	//   mineru —— 调用 MinerU HTTP 服务（OCR + 版面还原，返回 Markdown）。
	//
	// ★ pdf_parser=mineru 而 mineru.base_url 为空时，rag_service 启动即失败。
	//   不做"配置了就悄悄回退本地"的处理：那会让运维以为正文来自 MinerU，
	//   实际入库的是扫描件抽出来的空白文本，症状是"什么都搜不到"，
	//   而这种问题在检索质量上完全看不出根因。
	PDFParser string     `yaml:"pdf_parser"`
	MinerU    MinerUConf `yaml:"mineru"`

	// file_url 拉取配置（SSRF 防护相关）
	FetchTimeout       time.Duration `yaml:"fetch_timeout"`
	FetchMaxFileSizeMB int           `yaml:"fetch_max_file_size_mb"`
	// AllowPrivateIPs 允许拉取内网 IP（127.0.0.1/10.x/172.16-31/192.168）
	// 默认 false：file_url 不能指向内部服务，防止 SSRF
	AllowPrivateIPs bool `yaml:"allow_private_ips"`

	// ★ 降级开关
	KeywordFallbackEnabled bool `yaml:"keyword_fallback_enabled"`
	EnableRAG              bool `yaml:"enable_rag"`
	EnableIngest           bool `yaml:"enable_ingest"`

	// ---------- 混合检索（多来源）----------
	//
	// 设计：向量 / 关键词 / 网络 三来源并行召回，RRF 融合。
	// 任一来源故障不影响其他来源（防单源），
	// 全部来源失败才整体降级（degraded=true）。

	// MergeStrategy 融合策略。
	//   rrf         （默认）倒数排名融合：各来源按排名计分 1/(k+rank) 后合并去重
	//   vector_only 仅用向量来源（等价关闭混合，兼容旧行为）
	MergeStrategy string `yaml:"merge_strategy"`

	// EnableKeywordSearch 关键词检索是否作为常驻来源参与混合。
	//   true  关键词常驻（与向量并行召回，提升术语命中）
	//   false 仅故障时降级（等价 KeywordFallbackEnabled 语义）
	EnableKeywordSearch bool `yaml:"enable_keyword_search"`

	// EnableWebSearch 网络检索来源总开关。
	// 需要 WebSearch.Provider/Endpoint 已配置；未配置时自动跳过该来源。
	EnableWebSearch bool `yaml:"enable_web_search"`

	// WebSearch 网络检索配置。
	WebSearch WebSearchConf `yaml:"web_search"`

	// 补偿任务周期
	CompensateInterval time.Duration `yaml:"compensate_interval"`
	CompensateBatch    int           `yaml:"compensate_batch"`
}

// WebSearchConf 网络检索来源配置。
type WebSearchConf struct {
	// Provider 实现：mock（确定性假结果，无 key 环境/冒烟）或 http（真实搜索 API）
	Provider string `yaml:"provider"`
	// Endpoint 搜索 API URL，支持 {query} / {top_k} 占位符
	Endpoint string `yaml:"endpoint"`
	// APIKey 可选，非空时以 Authorization: Bearer 下发
	APIKey string `yaml:"api_key"`
	// Timeout 单次检索超时
	Timeout time.Duration `yaml:"timeout"`
	// TopK 默认返回条数
	TopK int `yaml:"top_k"`
}

// MergeStrategyRrf 融合策略常量。
const (
	MergeStrategyRRF        = "rrf"
	MergeStrategyVectorOnly = "vector_only"
)

// EffectiveMergeStrategy 返回生效的融合策略（空配置回落 rrf）。
func (c RAGConf) EffectiveMergeStrategy() string {
	switch c.MergeStrategy {
	case MergeStrategyVectorOnly:
		return MergeStrategyVectorOnly
	default:
		return MergeStrategyRRF
	}
}

// SourcesEnabled 返回常驻检索来源（向量恒启用）。
func (c RAGConf) SourcesEnabled() (keyword, web bool) {
	return c.EnableKeywordSearch, c.EnableWebSearch && c.WebSearch.Provider != ""
}

// AgentConf 智能体编排配置
type AgentConf struct {
	// 默认 Agent 参数（新建 Agent 时作为初值）
	DefaultModel       string  `yaml:"default_model"`
	DefaultTemperature float64 `yaml:"default_temperature"`
	DefaultMaxTokens   int     `yaml:"default_max_tokens"`
	DefaultTopK        int     `yaml:"default_top_k"`

	// SystemPrompt 缺省系统提示词。
	// Agent 未配置自己的 system_prompt 时用它。
	DefaultSystemPrompt string `yaml:"default_system_prompt"`

	// 上下文装配
	//
	// ★ HistoryTurns 决定带入多少轮历史。
	//   取值过大会撑爆模型上下文且成本线性上升；
	//   过小则模型"失忆"。默认 20 轮（约 4000 token 量级）是平衡点。
	HistoryTurns int `yaml:"history_turns"`
	// MaxContextChars 注入 RAG 上下文的字符上限。
	// 检索命中可能很长，必须截断，否则单条文档能吃掉整个窗口。
	MaxContextChars int `yaml:"max_context_chars"`

	// 单会话最多保留的消息数（超出后丢弃最旧的）
	MaxMessagesPerSession int `yaml:"max_messages_per_session"`
	// 单会话最多保留的会话数（按最后活跃时间淘汰）
	MaxSessionsPerUser int `yaml:"max_sessions_per_user"`

	// 空闲会话回收周期
	SessionTTL time.Duration `yaml:"session_ttl"`

	// RAG 默认开关
	EnableRAG bool `yaml:"enable_rag"`

	// 工具执行出站策略
	//
	// ★ 为什么默认严格：tools 表不是纯可信基础设施 —— admin 角色可通过
	//   POST /api/v1/tools 写入 endpoint（gateway/internal/router/router.go:198）。
	//   若允许内网，一个沦陷的 admin 账号就能让 agent_service 代为访问
	//   整段内网，包括 169.254.169.254 元数据服务。
	//   联调需要的 host.docker.internal 请走 ToolAllowHosts 逐个放行，
	//   不要整体放开。
	ToolAllowPrivateIPs bool `yaml:"tool_allow_private_ips"`
	// ToolAllowHosts 例外主机名单（精确或 "*.suffix"）
	ToolAllowHosts []string `yaml:"tool_allow_hosts"`
	// ToolFetchTimeout 单次工具调用的总上限，工具自身 timeout_ms 再小也受它约束
	ToolFetchTimeout time.Duration `yaml:"tool_fetch_timeout"`
	// ToolMaxResponseBytes 工具响应体上限，防止被一个超大响应拖垮内存
	ToolMaxResponseBytes int64 `yaml:"tool_max_response_bytes"`
}

// GatewayConf 网关配置
type GatewayConf struct {
	// 监听地址
	Addr string `yaml:"addr"`
	// ReadTimeout / WriteTimeout
	//
	// ★ WriteTimeout 必须为 0（或足够大）。
	//   SSE 流式响应会持续写入，默认 3s 的 WriteTimeout 会把
	//   长回答在传输中途掐断，客户端看到的是"回答到一半就断了"。
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`

	// 允许的跨域来源；["*"] 表示放行全部
	AllowOrigins []string `yaml:"allow_origins"`
	// 预检请求缓存时长
	MaxAge time.Duration `yaml:"max_age"`

	// 静态资源目录（前端构建产物）；为空则不启用
	StaticDir string `yaml:"static_dir"`

	// RPC 调用默认超时
	RPCTimeout time.Duration `yaml:"rpc_timeout"`
	// ChatStream 超时：流式整体上限，必须显著大于普通 RPC
	StreamTimeout time.Duration `yaml:"stream_timeout"`

	// 请求体大小上限
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

// MilvusConf 向量库
type MilvusConf struct {
	Addr       string        `yaml:"addr"`
	Username   string        `yaml:"username"`
	Password   string        `yaml:"password"`
	DBName     string        `yaml:"db_name"`
	Collection string        `yaml:"collection"`
	Dim        int           `yaml:"dim"`
	MetricType string        `yaml:"metric_type"` // COSINE / L2 / IP
	Timeout    time.Duration `yaml:"timeout"`

	// HNSW 参数
	HNSWM              int `yaml:"hnsw_m"`
	HNSWEfConstruction int `yaml:"hnsw_ef_construction"`
	HNSWEf             int `yaml:"hnsw_ef"`
}

// MinioConf 对象存储（RAG 文件原始副本 + 可下载地址）
//
// ★ 为什么不直接用 Milvus 内置的 MinIO（minio/minio 组件）：
//
//	Milvus 依赖的 MinIO 是它内部的存储层，不对外暴露 SDK 接口 ——
//	业务代码无法向那台 MinIO 写文件，只能另建独立实例（compose 里
//	aegis-minio 是业务用的；Milvus 那边是 milvus/minio 组件）。
type MinioConf struct {
	Endpoint  string        `yaml:"endpoint"`
	AccessKey string        `yaml:"access_key"`
	SecretKey string        `yaml:"secret_key"`
	Bucket    string        `yaml:"bucket"` // 默认 "aegis"
	UseSSL    bool          `yaml:"use_ssl"`
	Timeout   time.Duration `yaml:"timeout"`
}

// UpstreamConf 下游服务地址（本地开发直连；生产从 Nacos 发现）
type UpstreamConf struct {
	AuthService  string `yaml:"auth_service"`
	UserService  string `yaml:"user_service"`
	AgentService string `yaml:"agent_service"`
	LLMService   string `yaml:"llm_service"`
	RAGService   string `yaml:"rag_service"`
}

// InternalConf 服务间内部认证
//
// Token 非空时，所有 Kitex RPC 携带并校验共享令牌（见 pkg/internalauth）；
// 为空则认证关闭（本地 mock / 冒烟用，生产必须配置）。
type InternalConf struct {
	Token string `yaml:"token"`
}

// MetricsConf Prometheus 指标配置
type MetricsConf struct {
	// Enabled 为 true 时健康端口暴露 /metrics
	Enabled bool `yaml:"enabled"`
	// Path 指标路径，默认 /metrics
	Path string `yaml:"path"`
}

// OTelConf OpenTelemetry 追踪配置
//
// Endpoint 是 OTLP HTTP 协议的接收端（如 Jaeger / otel-collector），
// 服务把它当导出口而非「必须可用」的依赖：
// 导出失败只打日志（BatchSpanProcessor 自动重试），不拖垮业务。
type OTelConf struct {
	// Enabled 总开关。false 时全程 noop（零开销）
	Enabled bool `yaml:"enabled"`
	// Endpoint OTLP HTTP 端点，默认 http://127.0.0.1:4318
	Endpoint string `yaml:"endpoint"`
	// SamplerRatio 采样率 0-1，默认 1（全采样）
	SamplerRatio float64 `yaml:"sampler_ratio"`
}

// ---------- 加载 ----------

// Load 从 YAML 文件加载配置，并应用环境变量覆盖。
// 文件不存在时不报错，返回带默认值的配置（便于容器内纯环境变量启动）。
func Load(path string) (*Config, error) {
	c := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config %s: %w", path, err)
			}
		} else if err := yaml.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyEnv(c)
	return c, nil
}

// applyEnv 环境变量覆盖。约定前缀 AEGIS_，双下划线表示层级。
// 例：AEGIS_MOCK__ENABLED=true → mock.enabled
//
//	AEGIS_NACOS__ADDRS=1.2.3.4:8848,5.6.7.8:8848 → nacos.addrs（逗号分隔）
func applyEnv(c *Config) {
	setBool := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(key); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = b
			}
		}
	}
	setInt := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	setStr := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	setDur := func(key string, dst *time.Duration) {
		if v, ok := os.LookupEnv(key); ok {
			if d, err := time.ParseDuration(v); err == nil {
				*dst = d
			}
		}
	}
	setFloat := func(key string, dst *float64) {
		if v, ok := os.LookupEnv(key); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				*dst = f
			}
		}
	}
	setList := func(key string, dst *[]string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*dst = strings.Split(v, ",")
		}
	}
	setInt64 := func(key string, dst *int64) {
		if v, ok := os.LookupEnv(key); ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				*dst = n
			}
		}
	}

	// ★ 模型列表专用：按「索引」逐项覆盖。
	//
	// 为什么不直接把 models 整个塞进一个环境变量？
	//   Models 是结构体切片（每个模型有 name/base_url/api_key/priority 等10 个字段），
	//   序列化成一坨JSON 塞进 env，容器里既不可读也不好维护。
	//
	// 为什么不用 AEGIS_LLM__MODELS__<字段> 的通用反射式映射？
	//   本项目的 applyEnv 一贯是显式逐字段声明（见下方各处setXxx 调用）——
	//   显式优于隐式：新增字段时必须有人记得补映射，不会静默失效。
	//   所以这里同样显式声明「索引 → 字段」的对应关系。
	//
	// 索引与配置文件中 models 列表的顺序一致（0 起）。
	// 未设置的索引保持配置文件里的原值，因此允许「只注入密钥」这种用法。
	// ★ 环境变量驱动时可能完全没有 YAML 配置 ——
	// 先扫描有哪些 MODELS__N__NAME 被设置，按最大索引预分配数组，
	// 再逐个字段注入。这样 compose 里只设 env 也能正确生效。
	modelPrefix := "AEGIS_LLM__MODELS__"
	maxIdx := -1
	for _, env := range os.Environ() {
		k := strings.SplitN(env, "=", 2)[0]
		if !strings.HasPrefix(k, modelPrefix) {
			continue
		}
		rest := k[len(modelPrefix):] // "0__NAME" / "1__API_KEY" ...
		idxStr := strings.SplitN(rest, "__", 2)[0]
		if idx, err := strconv.Atoi(idxStr); err == nil && idx > maxIdx {
			maxIdx = idx
		}
	}
	if maxIdx >= 0 {
		if len(c.LLM.Models) <= maxIdx {
			grow := make([]ModelConf, maxIdx+1-len(c.LLM.Models))
			c.LLM.Models = append(c.LLM.Models, grow...)
		}
	}

	applyModelEnv := func(prefix string, idx int, m *ModelConf) {
		base := fmt.Sprintf("%s%d__", prefix, idx)
		setStr(base+"NAME", &m.Name)
		setStr(base+"PROVIDER", &m.Provider)
		setStr(base+"BASE_URL", &m.BaseURL)
		setStr(base+"API_KEY", &m.APIKey)
		setStr(base+"REAL_MODEL", &m.RealModel)
		setInt(base+"MAX_TOKENS", &m.MaxTokens)
		setFloat(base+"TEMPERATURE", &m.Temperature)
		setInt(base+"PRIORITY", &m.Priority)
		setStr(base+"STATUS", &m.Status)
		setBool(base+"SUPPORTS_TOOL_CALL", &m.SupportsToolCall)
		setBool(base+"SUPPORTS_VISION", &m.SupportsVision)
		setBool(base+"SUPPORTS_REASONING", &m.SupportsReasoning)
		setFloat(base+"INPUT_PRICE", &m.InputPrice)
		setFloat(base+"OUTPUT_PRICE", &m.OutputPrice)
		setStr(base+"RERANK_PATH", &m.RerankPath)
	}
	for i := range c.LLM.Models {
		applyModelEnv("AEGIS_LLM__MODELS__", i, &c.LLM.Models[i])
	}

	// 服务
	setStr("AEGIS_SERVICE__NAME", &c.Service.Name)
	setInt("AEGIS_SERVICE__PORT", &c.Service.Port)
	setStr("AEGIS_SERVICE__VERSION", &c.Service.Version)

	// 日志
	setStr("AEGIS_LOG__LEVEL", &c.Log.Level)
	setBool("AEGIS_LOG__JSON", &c.Log.JSON)

	// JSON 引擎
	setStr("AEGIS_JSON__ENGINE", &c.JSON.Engine)
	setBool("AEGIS_JSON__STRICT", &c.JSON.Strict)
	setBool("AEGIS_JSON__PRINT_ENGINE", &c.JSON.PrintEngine)

	// Nacos
	setBool("AEGIS_NACOS__ENABLED", &c.Nacos.Enabled)
	setList("AEGIS_NACOS__ADDRS", &c.Nacos.Addrs)
	setStr("AEGIS_NACOS__NAMESPACE", &c.Nacos.Namespace)
	setStr("AEGIS_NACOS__GROUP", &c.Nacos.Group)
	setStr("AEGIS_NACOS__DATA_ID", &c.Nacos.DataID)
	setBool("AEGIS_NACOS__REGISTER_ENABLED", &c.Nacos.RegisterEnabled)

	// ★ Mock（冒烟测试核心开关）
	setBool("AEGIS_MOCK__ENABLED", &c.Mock.Enabled)
	setInt("AEGIS_MOCK__LATENCY_MS", &c.Mock.LatencyMS)
	setInt("AEGIS_MOCK__FAIL_RATE", &c.Mock.FailRate)
	setList("AEGIS_MOCK__FAIL_TARGETS", &c.Mock.FailTargets)

	// 服务监听
	setStr("AEGIS_SERVER__RPC_ADDR", &c.Server.RPCAddr)
	setStr("AEGIS_SERVER__HTTP_ADDR", &c.Server.HTTPAddr)

	// 存储
	setStr("AEGIS_POSTGRES__DSN", &c.Postgres.DSN)
	setList("AEGIS_REDIS__ADDRS", &c.Redis.Addrs)
	setStr("AEGIS_REDIS__PASSWORD", &c.Redis.Password)
	setStr("AEGIS_S3__ENDPOINT", &c.S3.Endpoint)
	setStr("AEGIS_S3__BUCKET", &c.S3.Bucket)

	// 认证
	setStr("AEGIS_AUTH__JWT_SECRET", &c.Auth.JWTSecret)
	setStr("AEGIS_AUTH__JWT_PRIVATE_KEY", &c.Auth.JWTPrivateKey)
	setStr("AEGIS_AUTH__JWT_PUBLIC_KEY", &c.Auth.JWTPublicKey)
	setStr("AEGIS_AUTH__ISSUER", &c.Auth.Issuer)
	setList("AEGIS_AUTH__AUDIENCE", &c.Auth.Audience)
	setDur("AEGIS_AUTH__ACCESS_TOKEN_TTL", &c.Auth.AccessTokenTTL)

	// LLM
	setStr("AEGIS_LLM__PROVIDER", &c.LLM.Provider)
	setStr("AEGIS_LLM__DEFAULT_MODEL", &c.LLM.DefaultModel)
	setStr("AEGIS_LLM__DEFAULT_EMBED_MODEL", &c.LLM.DefaultEmbedModel)
	setInt("AEGIS_LLM__DEFAULT_EMBED_DIM", &c.LLM.DefaultEmbedDim)
	setBool("AEGIS_LLM__FALLBACK_DISABLED", &c.LLM.FallbackDisabled)

	// RAG
	setBool("AEGIS_RAG__ENABLE_RAG", &c.RAG.EnableRAG)
	setBool("AEGIS_RAG__ENABLE_INGEST", &c.RAG.EnableIngest)
	setBool("AEGIS_RAG__KEYWORD_FALLBACK_ENABLED", &c.RAG.KeywordFallbackEnabled)
	setInt("AEGIS_RAG__DEFAULT_TOP_K", &c.RAG.DefaultTopK)
	setFloat("AEGIS_RAG__DEFAULT_SCORE_THRESHOLD", &c.RAG.DefaultScoreThreshold)
	setInt("AEGIS_RAG__DEFAULT_CHUNK_SIZE", &c.RAG.DefaultChunkSize)
	setInt("AEGIS_RAG__DEFAULT_CHUNK_OVERLAP", &c.RAG.DefaultChunkOverlap)
	setInt("AEGIS_RAG__CONTEXT_WINDOW", &c.RAG.ContextWindow)
	setFloat("AEGIS_RAG__MOCK_SCORE_THRESHOLD", &c.RAG.MockScoreThreshold)
	setInt("AEGIS_RAG__INGEST_WORKERS", &c.RAG.IngestWorkers)

	// RAG 重排
	setBool("AEGIS_RAG__RERANK_ENABLED", &c.RAG.RerankEnabled)
	setStr("AEGIS_RAG__RERANK_MODEL", &c.RAG.RerankModel)
	setInt("AEGIS_RAG__RERANK_TOP_K", &c.RAG.RerankTopK)
	setFloat("AEGIS_RAG__RERANK_SCORE_THRESHOLD", &c.RAG.RerankScoreThreshold)

	// file_url 拉取（SSRF 防护）
	setDur("AEGIS_RAG__FETCH_TIMEOUT", &c.RAG.FetchTimeout)
	setInt("AEGIS_RAG__FETCH_MAX_FILE_SIZE_MB", &c.RAG.FetchMaxFileSizeMB)
	setBool("AEGIS_RAG__ALLOW_PRIVATE_IPS", &c.RAG.AllowPrivateIPs)
	setStr("AEGIS_RAG__PDF_PARSER", &c.RAG.PDFParser)
	setStr("AEGIS_RAG__MINERU__BASE_URL", &c.RAG.MinerU.BaseURL)
	setDur("AEGIS_RAG__MINERU__TIMEOUT", &c.RAG.MinerU.Timeout)
	setInt("AEGIS_RAG__MINERU__MAX_RESPONSE_MB", &c.RAG.MinerU.MaxResponseMB)
	setStr("AEGIS_RAG__MINERU__AUTH_TOKEN", &c.RAG.MinerU.AuthToken)

	// 混合检索（多来源）
	setStr("AEGIS_RAG__MERGE_STRATEGY", &c.RAG.MergeStrategy)
	setBool("AEGIS_RAG__ENABLE_KEYWORD_SEARCH", &c.RAG.EnableKeywordSearch)
	setBool("AEGIS_RAG__ENABLE_WEB_SEARCH", &c.RAG.EnableWebSearch)
	setStr("AEGIS_RAG__WEB_SEARCH__PROVIDER", &c.RAG.WebSearch.Provider)
	setStr("AEGIS_RAG__WEB_SEARCH__ENDPOINT", &c.RAG.WebSearch.Endpoint)
	setStr("AEGIS_RAG__WEB_SEARCH__API_KEY", &c.RAG.WebSearch.APIKey)
	setDur("AEGIS_RAG__WEB_SEARCH__TIMEOUT", &c.RAG.WebSearch.Timeout)
	setInt("AEGIS_RAG__WEB_SEARCH__TOP_K", &c.RAG.WebSearch.TopK)

	// Milvus
	setStr("AEGIS_MILVUS__ADDR", &c.Milvus.Addr)
	setStr("AEGIS_MILVUS__COLLECTION", &c.Milvus.Collection)
	setInt("AEGIS_MILVUS__DIM", &c.Milvus.Dim)
	// ★ 认证相关：Milvus 2.2+ 默认开启鉴权（默认用户 root/ Milvus），
	//   不注入 username/password 时连接会被拒。
	//   字段本来就存在于 MilvusConf，此前漏了env 映射 ——
	//   于是 docker-compose 里写了 AEGIS_MILVUS__USERNAME/PASSWORD
	//   却被静默忽略：容器里带着「配了鉴权」的样子连一个没鉴权的库。
	//   那种失败很难查——认证错误常表现为连接超时而非 401。
	setStr("AEGIS_MILVUS__USERNAME", &c.Milvus.Username)
	setStr("AEGIS_MILVUS__PASSWORD", &c.Milvus.Password)

	// MinIO
	setStr("AEGIS_MINIO__ENDPOINT", &c.Minio.Endpoint)
	setStr("AEGIS_MINIO__ACCESS_KEY", &c.Minio.AccessKey)
	setStr("AEGIS_MINIO__SECRET_KEY", &c.Minio.SecretKey)
	setStr("AEGIS_MINIO__BUCKET", &c.Minio.Bucket)
	setBool("AEGIS_MINIO__USE_SSL", &c.Minio.UseSSL)
	setDur("AEGIS_MINIO__TIMEOUT", &c.Minio.Timeout)

	// Agent
	setStr("AEGIS_AGENT__DEFAULT_MODEL", &c.Agent.DefaultModel)
	setFloat("AEGIS_AGENT__DEFAULT_TEMPERATURE", &c.Agent.DefaultTemperature)
	setInt("AEGIS_AGENT__DEFAULT_MAX_TOKENS", &c.Agent.DefaultMaxTokens)
	setInt("AEGIS_AGENT__DEFAULT_TOP_K", &c.Agent.DefaultTopK)
	setStr("AEGIS_AGENT__DEFAULT_SYSTEM_PROMPT", &c.Agent.DefaultSystemPrompt)
	setInt("AEGIS_AGENT__HISTORY_TURNS", &c.Agent.HistoryTurns)
	setInt("AEGIS_AGENT__MAX_CONTEXT_CHARS", &c.Agent.MaxContextChars)
	setInt("AEGIS_AGENT__MAX_MESSAGES_PER_SESSION", &c.Agent.MaxMessagesPerSession)
	setInt("AEGIS_AGENT__MAX_SESSIONS_PER_USER", &c.Agent.MaxSessionsPerUser)
	setDur("AEGIS_AGENT__SESSION_TTL", &c.Agent.SessionTTL)
	setBool("AEGIS_AGENT__ENABLE_RAG", &c.Agent.EnableRAG)
	setBool("AEGIS_AGENT__TOOL_ALLOW_PRIVATE_IPS", &c.Agent.ToolAllowPrivateIPs)
	setList("AEGIS_AGENT__TOOL_ALLOW_HOSTS", &c.Agent.ToolAllowHosts)
	setDur("AEGIS_AGENT__TOOL_FETCH_TIMEOUT", &c.Agent.ToolFetchTimeout)
	setInt64("AEGIS_AGENT__TOOL_MAX_RESPONSE_BYTES", &c.Agent.ToolMaxResponseBytes)

	// Gateway
	setStr("AEGIS_GATEWAY__ADDR", &c.Gateway.Addr)
	setDur("AEGIS_GATEWAY__READ_TIMEOUT", &c.Gateway.ReadTimeout)
	setDur("AEGIS_GATEWAY__WRITE_TIMEOUT", &c.Gateway.WriteTimeout)
	setDur("AEGIS_GATEWAY__IDLE_TIMEOUT", &c.Gateway.IdleTimeout)
	setList("AEGIS_GATEWAY__ALLOW_ORIGINS", &c.Gateway.AllowOrigins)
	setDur("AEGIS_GATEWAY__MAX_AGE", &c.Gateway.MaxAge)
	setStr("AEGIS_GATEWAY__STATIC_DIR", &c.Gateway.StaticDir)
	setDur("AEGIS_GATEWAY__RPC_TIMEOUT", &c.Gateway.RPCTimeout)
	setDur("AEGIS_GATEWAY__STREAM_TIMEOUT", &c.Gateway.StreamTimeout)
	setInt64("AEGIS_GATEWAY__MAX_BODY_BYTES", &c.Gateway.MaxBodyBytes)

	// 下游
	setStr("AEGIS_UPSTREAM__AUTH_SERVICE", &c.Upstream.AuthService)
	setStr("AEGIS_UPSTREAM__USER_SERVICE", &c.Upstream.UserService)
	setStr("AEGIS_UPSTREAM__AGENT_SERVICE", &c.Upstream.AgentService)
	setStr("AEGIS_UPSTREAM__LLM_SERVICE", &c.Upstream.LLMService)
	setStr("AEGIS_UPSTREAM__RAG_SERVICE", &c.Upstream.RAGService)

	// 内部认证
	setStr("AEGIS_INTERNAL__TOKEN", &c.Internal.Token)

	// 指标
	setBool("AEGIS_METRICS__ENABLED", &c.Metrics.Enabled)
	setStr("AEGIS_METRICS__PATH", &c.Metrics.Path)

	// 追踪
	setBool("AEGIS_OTEL__ENABLED", &c.OTel.Enabled)
	setStr("AEGIS_OTEL__ENDPOINT", &c.OTel.Endpoint)
	setFloat("AEGIS_OTEL__SAMPLER_RATIO", &c.OTel.SamplerRatio)
}

// Default 返回一份可直接启动的默认配置。
// 默认值偏向"本地开发 + mock 友好"：不连 Nacos，PG/Milvus 指向 localhost。
func Default() *Config {
	return &Config{
		Service: ServiceConf{Version: "dev"},
		Log:     LogConf{Level: "info", JSON: false},
		JSON: JSONConf{
			// auto = 用编译进来的引擎，不干预
			Engine:      JSONEngineAuto,
			Strict:      false,
			PrintEngine: true,
		},
		Nacos: NacosConf{
			Enabled:         false, // 默认关闭，避免本地开发强依赖
			Addrs:           []string{"127.0.0.1:8848"},
			Namespace:       "public",
			Group:           "DEFAULT_GROUP",
			DialTimeout:     "5s",
			RegisterEnabled: false,
			Weight:          10,
			CacheDir:        "/tmp/nacos/cache",
		},
		Mock: MockConf{
			Enabled:   false, // ★ 显式开启才进入 mock 模式
			LatencyMS: 0,
			FailRate:  0,
		},
		Server: ServerConf{
			RPCAddr: "0.0.0.0",
			// HTTPAddr 留空：由各服务通过 serverx.Options.DefaultHTTPPort 声明，
			// 避免这里预填值把服务级默认端口覆盖掉。
			HTTPAddr:        "",
			ShutdownTimeout: 10 * time.Second,
		},
		Postgres: PostgresConf{
			DSN:             "postgres://aegis:aegis@127.0.0.1:5432/aegis?sslmode=disable",
			MaxOpenConns:    20,
			MaxIdleConns:    5,
			ConnMaxLifetime: time.Hour,
			ConnMaxIdleTime: 10 * time.Minute,
			QueryTimeout:    5 * time.Second,
		},
		Redis: RedisConf{
			Addrs:    []string{"127.0.0.1:6379"},
			DB:       0,
			PoolSize: 20,
		},
		S3: S3Conf{
			Endpoint:      "127.0.0.1:9000",
			Bucket:        "aegis-docs",
			PresignExpire: 15 * time.Minute,
		},
		Auth: AuthConf{
			Issuer:               "aegis",
			Audience:             []string{"aegis"},
			AccessTokenTTL:       2 * time.Hour,
			RefreshTokenTTL:      7 * 24 * time.Hour,
			CaptchaAfterFailures: 3,
			MaxLoginFailures:     5,
			LockDuration:         15 * time.Minute,
		},
		LLM: LLMConf{
			Provider:          "eino",
			DefaultModel:      "deepseek-chat",
			DefaultEmbedModel: "bge-m3",
			DefaultEmbedDim:   1024,
			Timeout:           60 * time.Second,
			MaxTokens:         4096,
			CacheEnabled:      true,
			EmbedCacheTTL:     24 * time.Hour,
		},
		RAG: RAGConf{
			DefaultChunkSize:      800,
			DefaultChunkOverlap:   100,
			DefaultTopK:           5,
			DefaultScoreThreshold: 0.3,
			// mock 用词袋模拟，相似度上限约 0.25，需单独阈值（见字段注释）
			MockScoreThreshold: 0.1,
			// 重排默认关闭：它是精度增强项，且依赖厂商提供 rerank 端点。
			// 关闭时链路退化为「向量 topK + 关键词降级」，功能完整。
			RerankEnabled:        true, // ★ 默认开启
			RerankTopK:           20,
			RerankScoreThreshold: 0.3,
			ContextWindow:        1,
			IngestWorkers:        4,
			MaxFileSizeMB:        50,
			MaxPages:             500,
			// 与 pkg/parser 实际支持的格式保持一致：
			// 白名单放进 parser 不支持的类型，文件能上传但必然解析失败，
			// 用户看到的只有一句"解析失败"，排查成本极高。
			AllowedTypes: []string{
				"pdf", "docx", "pptx", "xlsx",
				"html", "htm", "csv", "md", "markdown", "txt", "json",
			},
			// PDF 默认走本地解析（无外部依赖，代价是不支持扫描件）
			PDFParser: "local",
			MinerU: MinerUConf{
				BaseURL:       "",
				Timeout:       120 * time.Second,
				MaxResponseMB: 32,
			},
			// file_url 拉取：10s 超时 + 20MB 上限 + 默认拒绝内网 IP（SSRF 防护）
			FetchTimeout:           10 * time.Second,
			FetchMaxFileSizeMB:     20,
			AllowPrivateIPs:        false,
			KeywordFallbackEnabled: true, // ★ 默认开启降级
			EnableRAG:              true,
			EnableIngest:           true,
			// 混合检索默认：RRF 融合 + 关键词常驻；网络检索默认关闭（需配置端点）
			MergeStrategy:       MergeStrategyRRF,
			EnableKeywordSearch: true,
			EnableWebSearch:     false,
			WebSearch:           WebSearchConf{Provider: "mock", TopK: 5, Timeout: 5 * time.Second},
			CompensateInterval:  5 * time.Minute,
			CompensateBatch:     1000,
		},
		Milvus: MilvusConf{
			Addr:               "127.0.0.1:19530",
			DBName:             "default",
			Collection:         "aegis_chunks",
			Dim:                1024,
			MetricType:         "COSINE",
			Timeout:            10 * time.Second,
			HNSWM:              16,
			HNSWEfConstruction: 200,
			HNSWEf:             64,
		},
		Agent: AgentConf{
			DefaultModel:       "deepseek-chat",
			DefaultTemperature: 0.7,
			DefaultMaxTokens:   4096,
			DefaultTopK:        5,
			DefaultSystemPrompt: "你是一个严谨、乐于助人的 AI 助手。" +
				"回答时优先使用检索到的资料并在句末标注引用编号（如 [1]）；" +
				"资料不足时明确说明，不要编造。",
			HistoryTurns:          20,
			MaxContextChars:       4000,
			MaxMessagesPerSession: 500,
			MaxSessionsPerUser:    200,
			SessionTTL:            7 * 24 * time.Hour,
			EnableRAG:             true,
			// 工具出站默认严格：不放开内网、例外名单为空。
			// 联调请在 configs/*.yaml 里显式加 tool_allow_hosts。
			ToolAllowPrivateIPs:  false,
			ToolAllowHosts:       nil,
			ToolFetchTimeout:     30 * time.Second,
			ToolMaxResponseBytes: 1 << 20, // 1MB
		},
		Gateway: GatewayConf{
			// 8000 是对外统一入口，dev/生产一致：
			// 端口写死两处（配置默认值 + 文档）必然漂移。
			Addr: ":8000",
			// ★ WriteTimeout 默认为 0（不限制）。
			//   SSE 长回答会被默认超时从中途掐断，
			//   客户端看到的是"回答到一半断了"，这类问题极难定位。
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 0,
			IdleTimeout:  120 * time.Second,
			AllowOrigins: []string{"*"},
			MaxAge:       12 * time.Hour,
			RPCTimeout:   30 * time.Second,
			// 流式整体上限：一次长回答允许写多久
			StreamTimeout: 10 * time.Minute,
			MaxBodyBytes:  4 << 20, // 4MB
		},
		Upstream: UpstreamConf{
			AuthService: "127.0.0.1:8881",
			UserService: "127.0.0.1:8882",
			// ★ 8886：agent_service 的实际端口。
			//   曾经误写 8883（无服务占用），表现为网关启动正常
			//   但每个请求都 connection refused —— 很难第一眼定位。
			AgentService: "127.0.0.1:8886",
			LLMService:   "127.0.0.1:8884",
			RAGService:   "127.0.0.1:8885",
		},
		// 指标默认开启：/metrics 挂在健康端口上，本地开发与生产都该可观测。
		Metrics: MetricsConf{
			Enabled: true,
			Path:    "/metrics",
		},
		// 追踪默认关闭：需要部署 Jaeger/collector 才有意义。
		// compose 的 obs profile 提供整套接收端（见 docker-compose.yml）。
		OTel: OTelConf{
			Enabled:      false,
			Endpoint:     "http://127.0.0.1:4318",
			SamplerRatio: 1,
		},
	}
}
