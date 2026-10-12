// Package auth 提供网关侧的令牌校验适配与健康聚合。
//
// 为什么不在 middleware 里直接调 auth_service：
//
//	中间件是所有请求的必经路径，把 RPC 调用塞进来会让
//	「路由注册」与「下游依赖」耦合，无法独立测试。
//	这里用窄接口注入，中间件只负责取令牌与判断放行。
package auth

import (
	"context"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	authv1 "github.com/aegis/idl/kitex_gen/authv1"
	authservice "github.com/aegis/idl/kitex_gen/authv1/authservice"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"

	"github.com/aegis/gateway/internal/handler"
	"github.com/aegis/pkg/logger"
)

// Verifier 把 auth_service.VerifyToken 适配成中间件需要的窄接口。
type Verifier struct {
	client authservice.Client
}

// NewVerifier 构造校验器
func NewVerifier(c authservice.Client) *Verifier {
	return &Verifier{client: c}
}

// Verify 校验令牌并返回调用方身份。
//
// requiredScope 为空表示只验有效性，不要求特定角色。
func (v *Verifier) Verify(ctx context.Context, accessToken, requiredScope string) (*commonv1.Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := v.client.VerifyToken(ctx, &authv1.VerifyTokenReq{
		AccessToken:   accessToken,
		RequiredScope: requiredScope,
	})
	if err != nil {
		return nil, err
	}
	if !resp.GetValid() {
		return nil, errTokenInvalid
	}
	return &commonv1.Identity{
		Uid:      resp.GetUid(),
		Username: resp.GetUsername(),
		Tenant:   resp.GetTenant(),
		Scopes:   resp.GetScopes(),
	}, nil
}

type tokenError string

func (e tokenError) Error() string { return string(e) }

const errTokenInvalid = tokenError("token invalid")

// ---------- 健康聚合 ----------

type probe struct {
	name string
	call func(context.Context) error
}

// CheckAll 并发探活全部下游。
//
// ★ 必须并发：串行探活时最慢的那个决定总耗时，
//
//	任一下游挂起都会拖垮网关的健康检查端点，
//	进而让 K8s 把网关也判定为不健康并摘流 ——
//	一个下游的故障不该导致整个平台级联下线。
func CheckAll(ctx context.Context, c *app.RequestContext, d *handler.Deps) {
	//★ 不探 llm_service：
	//   gateway 不直连它（统一经 agent_service 转发），
	//   且它没有独立的健康端口，硬探只能靠业务调用，
	//   那会触发真实推理/计费。
	timeout := 3 * time.Second
	inner, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	probes := []probe{
		{"auth_service", func(context.Context) error {
			_, err := d.Auth.Health(inner, &commonv1.HealthReq{})
			return err
		}},
		{"user_service", func(context.Context) error {
			_, err := d.User.Health(inner, &commonv1.HealthReq{})
			return err
		}},

		{"rag_service", func(context.Context) error {
			_, err := d.RAG.Health(inner, &commonv1.HealthReq{})
			return err
		}},
	}

	results := make(map[string]string, len(probes))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, p := range probes {
		wg.Add(1)
		go func(p probe) {
			defer wg.Done()
			err := p.call(inner)
			status := "ok"
			if err != nil {
				status = "down: " + err.Error()
				logger.Warn(inner, "下游健康检查失败", "target", p.name, "err", err)
			}
			mu.Lock()
			results[p.name] = status
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	// 只要有一个不通就整体报 503，
	// 让上游负载均衡能感知并停止转发
	healthy := true
	for _, v := range results {
		if v != "ok" {
			healthy = false
			break
		}
	}

	status := consts.StatusOK
	if !healthy {
		status = consts.StatusServiceUnavailable
	}
	c.JSON(status, utils.H{
		"healthy":    healthy,
		"service":    "gateway",
		"downstream": results,
	})
}
