// Package internalauth 提供服务间 Kitex RPC 的内部令牌认证。
//
// # 为什么需要它
//
// gateway 已有面向用户的 JWT 鉴权，但服务间 RPC（gateway→agent、
// agent→rag→llm、auth→user）此前完全裸奔：任何一个能接入 Compose
// 网络的容器都可以直接调用任意下游，并且下游 handler 无条件信任
// 请求里的 Identity 字段 —— 伪造 uid 即可越权读写他人数据。
//
// # 机制
//
// 轻量共享密钥（对称令牌）：
//   - client 侧：中间件把令牌写入 TTHeader 元数据（metainfo），
//     随每次 RPC 透传；
//   - server 侧：中间件校验元数据里的令牌，不匹配直接拒绝。
//
// 配置 AEGIS_INTERNAL__TOKEN（conf.Internal.Token）：
//   - 为空 → 认证关闭（本地 mock / 冒烟保持可用）；
//   - 非空 → 双方必须一致，否则客户端调用全部被拒。
//
// # 边界
//
// 这不是 mTLS，防的是「同网络内的未授权调用方」，不防「网络内
// 已被攻破的主机抓包取钥」。传输层加密由部署侧（K8s NetworkPolicy
// / service mesh）负责，职责不重叠。
package internalauth

import (
	"context"
	"crypto/subtle"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/endpoint"
	npmeta "github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/metadata"
	"github.com/cloudwego/kitex/pkg/transmeta"
	"github.com/cloudwego/kitex/server"
	"github.com/cloudwego/kitex/transport"

	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/logger"
)

// HeaderToken 令牌在 TTHeader 元数据中的键。
//
// ★ 必须小写：TTHeader 对键名大小写敏感，client/server 两边
// 写成不同大小写会导致「明明配了令牌却校验失败」的静默故障。
const HeaderToken = "x-aegis-internal-token"

// cgiTokenKey gRPC（HTTP2）通道下令牌在 metainfo persistent 桶里的键。
//
// gRPC 通道：令牌以 HTTP2 header `rpc-persist-x-aegis-internal-token`
// 过线，服务端 MetainfoServerHandler.OnReadStream 经 metainfo.
// FromHTTPHeader 恢复时会把键转成 CGI 变量格式（gopkg http.go:
// HTTPHeaderToCGIVariable：小写连字符 → 大写下划线），即本常量。
// TTHeader 通道不做该转换，键保持 HeaderToken 原样 —— 因此服务端
// 两个键都要读（见 ServerMiddleware）。
var cgiTokenKey = metainfo.HTTPHeaderToCGIVariable(HeaderToken)

// ServerMiddleware 构造 server 侧校验中间件。
//
// token 为空时返回 nil（认证关闭），调用方据此决定是否挂载，
// 避免为本地 mock 模式维护两条装配路径。
func ServerMiddleware(token string) endpoint.Middleware {
	if token == "" {
		return nil
	}
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			// ★ 令牌经 WithPersistentValues 注入、以 RPC_PERSIST_ 前缀
			//   过线，落进 metainfo 的 persistent 桶 ——
			// TTHeader 通道恢复键 = HeaderToken 原样；gRPC（HTTP2）通道
			// 经 FromHTTPHeader 恢复成 CGI 格式（见 cgiTokenKey 注释）。
			// 两个桶都在 persistent，依次读取即可。
			v, ok := metainfo.GetPersistentValue(ctx, HeaderToken)
			if !ok || v == "" {
				v, ok = metainfo.GetPersistentValue(ctx, cgiTokenKey)
			}
			if !ok || v == "" {
				// 诊断：persistent 桶里到底有什么（避免静默故障无从排查）
				logger.Warn(ctx, "内部认证失败: persistent 桶缺少令牌",
					"persistent_keys", metainfo.GetAllPersistentValues(ctx))
				return errno.Newf(errno.ErrUnauthorized, "缺少内部认证令牌")
			}
			// constant-time 比较：避免令牌逐字节比对在时序上泄露信息
			if subtle.ConstantTimeCompare([]byte(v), []byte(token)) != 1 {
				return errno.Newf(errno.ErrUnauthorized, "内部认证令牌不匹配")
			}
			return next(ctx, req, resp)
		}
	}
}

// ClientOptions 构造 client 侧注入令牌的选项。
//
// token 为空时返回 nil，调用方用 append(opts, ...) 零成本接线：
//
//	opts := []client.Option{client.WithHostPorts(addr)}
//	opts = append(opts, internalauth.ClientOptions(c.Internal.Token)...)
func ClientOptions(token string) []client.Option {
	if token == "" {
		return nil
	}
	mw := func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			// 通道 1：metainfo persistent 桶 —— TTHeader/thrift 路径
			// 的唯一通道。PersistentValues 会沿调用链继续向更下游
			// 透传（gateway→agent→rag→llm 每一跳叠加，幂等）。
			ctx = metainfo.WithPersistentValues(ctx, HeaderToken, token)
			// 通道 2：gRPC（HTTP2）路径 —— 服务里含 streaming 方法的
			// 生成代码（如 agentv1）会强制 gRPC 传输并覆盖本包设置的
			// TTHeader，而 Kitex 对 pb+gRPC unary **不做** metainfo→
			// HTTP header 的桥接（桥接只在 streaming 建连时做一次），
			// metainfo 桶里的值根本发不出去。因此必须用 gRPC 标准
			// metadata 直接写 HTTP2 header；键加 rpc-persist- 前缀，
			// 服务端 MetainfoServerHandler 的 OnReadStream 会识别该
			// 前缀并恢复到 metainfo persistent 桶 —— 两通道在服务端
			// 汇聚为同一读取路径（GetPersistentValue）。
			// 对 TTHeader/thrift 服务此注入无副作用（不读 gRPC metadata）。
			ctx = npmeta.AppendToOutgoingContext(ctx, metainfo.HTTPPrefixPersistent+HeaderToken, token)
			return next(ctx, req, resp)
		}
	}
	// ★ Kitex 两个易混淆的 MetaHandler（探针实测）：
	//   - ClientTTHeaderHandler/ServerTTHeaderHandler：只写协议内部字段
	//     （from/to service、超时等），不携带 ctx metainfo；
	//   - MetainfoClientHandler/MetainfoServerHandler：才是 ctx metainfo
	//     ⇋ TTHeader strInfo 的序列化器，必须挂这对。
	//   另外默认传输（framed）不携带 TransInfo，必须切 TTHeader 传输。
	return []client.Option{
		client.WithMiddleware(mw),
		client.WithMetaHandler(transmeta.MetainfoClientHandler),
		client.WithTransportProtocol(transport.TTHeader),
	}
}

// ServerOptions 服务端装配选项（中间件 + TTHeader 元信息解码器）。
//
// serverx.buildKitexServer 统一装配用。kitex 的 server.Option 是
// struct（非 func），没有「空值」语义，故认证关闭时返回 nil，
// 调用方 append(opts, ...) 即可零成本接线。
func ServerOptions(token string) []server.Option {
	mw := ServerMiddleware(token)
	if mw == nil {
		return nil
	}
	// MetainfoServerHandler 把 TTHeader strInfo 解回 ctx
	//（否则服务端 metainfo 恒为空，见 ClientOptions 注释）
	return []server.Option{
		server.WithMiddleware(mw),
		server.WithMetaHandler(transmeta.MetainfoServerHandler),
	}
}

// Enabled 报告给定配置下认证是否生效（启动横幅/排查用）
func Enabled(token string) bool { return token != "" }

// WarnIfDisabled 认证关闭时在启动日志中显式告警。
//
// 关闭是合法状态（本地 mock），但必须留痕 ——
// 生产环境带着空令牌上线而无人知晓，等于安全开关形同虚设。
func WarnIfDisabled(ctx context.Context, token, service string) {
	if !Enabled(token) {
		logger.Warn(ctx, "内部 RPC 认证未启用（internal.token 为空），同网络内任何调用方均可直连",
			"service", service)
	}
}
