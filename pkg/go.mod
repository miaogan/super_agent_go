module github.com/aegis/pkg

go 1.26.0

require (
	// ★ sonic 是构建标签门控的依赖（见 pkg/jsonx）。
	//
	//   只有 go build -tags sonic 时才真正编译它，
	//   默认构建走 encoding/json，不需要它。
	//
	//   之所以必须显式 require 而非留作 indirect：
	//   `go mod tidy` 不识别自定义构建标签，会判定 sonic 未被使用
	//   而将其降级为 indirect 甚至删除。显式声明可避免
	//   下一个人跑完 tidy 之后 sonic 构建突然失败。
	github.com/bytedance/sonic v1.15.0
	gopkg.in/yaml.v3 v3.0.1
)

// ★ Milvus 3.x 适配层（pkg/vectordb/milvus3，ADR-006）
//
//	为什么锁 client/v3 **v3.0.0** 而不是文档里曾写的 v3.0.2：
//	  - 实测 module proxy 上 client/v3 只有 v3.0.0 与 v3.0.0-beta，
//	    **v3.0.2 不存在**（那是服务端 milvusdb/milvus 的版本号，
//	    服务端与客户端是两条独立的版本线）。
//	  - v3.0.0 发布于 2026-09-11，是 client/v3 的首个正式 GA。
//	若照抄文档里的 v3.0.2，`go get` 会直接报「no matching versions」，
//	而错误信息不会提示「你是不是想写服务端版本」。
//
//	为什么不用 client/v2 + Milvus 2.x：
//	ADR-006 已论证过——放弃 2.6 是为跟进 3.0 主线，
//	且服务端 compose 已锁 v3.0.2，客户端必须与之匹配。
//
//	代价：引入 grpc/otel 等一批传递依赖，见下方 exclude 说明。
require github.com/milvus-io/milvus/client/v3 v3.0.0

// ---------- 依赖冲突处理：genproto ambiguous import ----------
//
//	现象：编译报
//	  ambiguous import: found package
//	  google.golang.org/genproto/googleapis/rpc/status in multiple modules
//
//	原因：`googleapis/rpc/status` 这个包在两个模块里同时存在——
//	  · google.golang.org/genproto（老版，内含该包）
//	  · google.golang.org/genproto/googleapis/rpc（新版拆分模块）
//	grpc v1.80.0 只认拆分后的那个，但 otel / pingcap 等依赖链
//	按MVS 把老版 genproto 也拉进了构建列表，于是两个都可见 → 歧义。
//
//	为什么用「升级 genproto」而不是「逐个 exclude 老版本」：
//	老版本来自多条互不相干的依赖链，每条带的版本号都不同
//	（实测依次为 20200526/ 20210513 / 20210624 / 20190819……），
//	exclude 要跟着每条链的解析结果不断追加，
//	任何人动一下依赖就可能又冒出来一个新版本 —— 那是打地鼠。
//
//	正解：把 genproto 提到**拆分之后的版本**（2026-10 起的那批），
//	该版本已不含 googleapis/rpc/status，歧义自然消失，
//	且是对MVS 结果的正当提升，不需要维护 exclude 列表。
//
//	★ 为什么这个提升是安全的：
//	grpc v1.80.0 要求的是 googleapis/rpc（拆分模块），
//	对老 genproto 已无要求；而本项目自身只用 kitex，
//	kitex v0.16.3 走 grpc 的公开 API，不触碰 genproto 内部包。
//	真正被编译的status 始终来自拆分模块，与 grpc 期望一致。
require (
	google.golang.org/genproto v0.0.0-20261005182115-fad411399dd8
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc
)

require (
	github.com/bytedance/gopkg v0.1.4 // indirect
	github.com/bytedance/sonic/loader v0.5.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/cloudwego/kitex v0.16.3 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/goccy/go-json v0.10.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/klauspost/compress v1.20.1 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/ledongthuc/pdf v0.0.0-20260907135840-6c8c28e0e8a0 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/minio/minio-go/v7 v7.0.76 // indirect
	github.com/prometheus/client_golang v1.25.0 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/richardlehane/mscfb v1.0.7 // indirect
	github.com/richardlehane/msoleps v1.0.6 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/tiendc/go-deepcopy v1.7.2 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/xuri/efp v0.0.1 // indirect
	github.com/xuri/excelize/v2 v2.11.0 // indirect
	github.com/xuri/nfp v0.0.2-0.20250530014748-2ddeb826f9a9 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	golang.org/x/arch v0.14.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
