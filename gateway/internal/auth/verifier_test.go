package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/kitex/client/callopt"

	authv1 "github.com/aegis/idl/kitex_gen/authv1"
	"github.com/aegis/idl/kitex_gen/authv1/authservice"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"
	"github.com/aegis/idl/kitex_gen/ragv1/ragservice"
	userv1 "github.com/aegis/idl/kitex_gen/userv1"
	"github.com/aegis/idl/kitex_gen/userv1/userservice"

	"github.com/aegis/gateway/internal/handler"
)

// ---------- 可编程桩 ----------

// stubAuthClient 可编程的 authservice.Client，只覆盖 VerifyToken 与 Health。
type stubAuthClient struct {
	verifyResp *authv1.VerifyTokenResp
	verifyErr  error
	gotReq     *authv1.VerifyTokenReq
	healthErr  error
}

var _ authservice.Client = (*stubAuthClient)(nil)

func (s *stubAuthClient) VerifyToken(_ context.Context, req *authv1.VerifyTokenReq,
	_ ...callopt.Option) (*authv1.VerifyTokenResp, error) {
	s.gotReq = req
	if s.verifyErr != nil {
		return nil, s.verifyErr
	}
	return s.verifyResp, nil
}

func (s *stubAuthClient) Health(_ context.Context, _ *commonv1.HealthReq,
	_ ...callopt.Option) (*commonv1.HealthResp, error) {
	if s.healthErr != nil {
		return nil, s.healthErr
	}
	return &commonv1.HealthResp{}, nil
}

func (s *stubAuthClient) Login(context.Context, *authv1.LoginReq, ...callopt.Option) (*authv1.LoginResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubAuthClient) RefreshToken(context.Context, *authv1.RefreshTokenReq, ...callopt.Option) (*authv1.LoginResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubAuthClient) Logout(context.Context, *authv1.LogoutReq, ...callopt.Option) (*commonv1.OpResult, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubAuthClient) OAuthAuthorize(context.Context, *authv1.OAuthAuthorizeReq, ...callopt.Option) (*authv1.OAuthAuthorizeResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubAuthClient) OAuthCallback(context.Context, *authv1.OAuthCallbackReq, ...callopt.Option) (*authv1.LoginResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubAuthClient) ListIdPs(context.Context, *authv1.ListIdPsReq, ...callopt.Option) (*authv1.ListIdPsResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubAuthClient) CheckPermission(context.Context, *authv1.CheckPermissionReq, ...callopt.Option) (*authv1.CheckPermissionResp, error) {
	return nil, errors.New("stub 未实现")
}

// stubUserClient 只实现 Health。
type stubUserClient struct {
	healthErr error
}

var _ userservice.Client = (*stubUserClient)(nil)

func (s *stubUserClient) Health(_ context.Context, _ *commonv1.HealthReq,
	_ ...callopt.Option) (*commonv1.HealthResp, error) {
	if s.healthErr != nil {
		return nil, s.healthErr
	}
	return &commonv1.HealthResp{}, nil
}

func (s *stubUserClient) GetUser(context.Context, *userv1.GetUserReq, ...callopt.Option) (*userv1.GetUserResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubUserClient) BatchGetUsers(context.Context, *userv1.BatchGetUsersReq, ...callopt.Option) (*userv1.BatchGetUsersResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubUserClient) CreateUser(context.Context, *userv1.CreateUserReq, ...callopt.Option) (*userv1.CreateUserResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubUserClient) UpdateUser(context.Context, *userv1.UpdateUserReq, ...callopt.Option) (*userv1.UpdateUserResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubUserClient) DeleteUser(context.Context, *userv1.DeleteUserReq, ...callopt.Option) (*commonv1.OpResult, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubUserClient) ListUsers(context.Context, *userv1.ListUsersReq, ...callopt.Option) (*userv1.ListUsersResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubUserClient) GetUserCredential(context.Context, *userv1.GetUserCredentialReq, ...callopt.Option) (*userv1.GetUserCredentialResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubUserClient) GetPreference(context.Context, *userv1.GetPreferenceReq, ...callopt.Option) (*userv1.GetPreferenceResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubUserClient) SetPreference(context.Context, *userv1.SetPreferenceReq, ...callopt.Option) (*commonv1.OpResult, error) {
	return nil, errors.New("stub 未实现")
}

// stubRAGClient 只实现 Health。
type stubRAGClient struct {
	healthErr error
}

var _ ragservice.Client = (*stubRAGClient)(nil)

func (s *stubRAGClient) Health(_ context.Context, _ *commonv1.HealthReq,
	_ ...callopt.Option) (*commonv1.HealthResp, error) {
	if s.healthErr != nil {
		return nil, s.healthErr
	}
	return &commonv1.HealthResp{}, nil
}

func (s *stubRAGClient) CreateKnowledgeBase(context.Context, *ragv1.CreateKnowledgeBaseReq, ...callopt.Option) (*ragv1.CreateKnowledgeBaseResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) GetKnowledgeBase(context.Context, *ragv1.GetKnowledgeBaseReq, ...callopt.Option) (*ragv1.GetKnowledgeBaseResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) ListKnowledgeBases(context.Context, *ragv1.ListKnowledgeBasesReq, ...callopt.Option) (*ragv1.ListKnowledgeBasesResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) UpdateKnowledgeBase(context.Context, *ragv1.UpdateKnowledgeBaseReq, ...callopt.Option) (*ragv1.UpdateKnowledgeBaseResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) DeleteKnowledgeBase(context.Context, *ragv1.DeleteKnowledgeBaseReq, ...callopt.Option) (*commonv1.OpResult, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) UploadDocument(context.Context, *ragv1.UploadDocumentReq, ...callopt.Option) (*ragv1.UploadDocumentResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) GetDocument(context.Context, *ragv1.GetDocumentReq, ...callopt.Option) (*ragv1.GetDocumentResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) ListDocuments(context.Context, *ragv1.ListDocumentsReq, ...callopt.Option) (*ragv1.ListDocumentsResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) DeleteDocument(context.Context, *ragv1.DeleteDocumentReq, ...callopt.Option) (*commonv1.OpResult, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) RetryDocument(context.Context, *ragv1.RetryDocumentReq, ...callopt.Option) (*commonv1.OpResult, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) ListChunks(context.Context, *ragv1.ListChunksReq, ...callopt.Option) (*ragv1.ListChunksResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) Retrieve(context.Context, *ragv1.RetrieveReq, ...callopt.Option) (*ragv1.RetrieveResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) DebugRetrieve(context.Context, *ragv1.DebugRetrieveReq, ...callopt.Option) (*ragv1.DebugRetrieveResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) BindAgentKB(context.Context, *ragv1.BindAgentKBReq, ...callopt.Option) (*commonv1.OpResult, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubRAGClient) ListAgentKB(context.Context, *ragv1.ListAgentKBReq, ...callopt.Option) (*ragv1.ListAgentKBResp, error) {
	return nil, errors.New("stub 未实现")
}

// ---------- Verify ----------

// TestVerifyValid 有效令牌应返回身份，且把 token 与 scope 透传给下游。
func TestVerifyValid(t *testing.T) {
	stub := &stubAuthClient{verifyResp: &authv1.VerifyTokenResp{
		Valid:    true,
		Uid:      42,
		Username: "alice",
		Tenant:   "t1",
		Scopes:   []string{"user", "admin"},
	}}
	v := NewVerifier(stub)

	id, err := v.Verify(context.Background(), "tok-1", "admin")
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if id.GetUid() != 42 || id.GetUsername() != "alice" || id.GetTenant() != "t1" {
		t.Errorf("身份字段不符: %+v", id)
	}
	if len(id.GetScopes()) != 2 {
		t.Errorf("scopes 透传不符: %v", id.GetScopes())
	}
	if stub.gotReq.GetAccessToken() != "tok-1" || stub.gotReq.GetRequiredScope() != "admin" {
		t.Errorf("请求参数透传不符: %+v", stub.gotReq)
	}
}

// TestVerifyInvalid 下游判定无效 → 返回 token invalid。
func TestVerifyInvalid(t *testing.T) {
	stub := &stubAuthClient{verifyResp: &authv1.VerifyTokenResp{Valid: false, Reason: "过期"}}
	v := NewVerifier(stub)

	_, err := v.Verify(context.Background(), "bad", "")
	if err == nil || err.Error() != "token invalid" {
		t.Fatalf("应返回 token invalid, 实际 %v", err)
	}
}

// TestVerifyDownstreamError 下游 RPC 报错 → 原样透传（不吞错误）。
func TestVerifyDownstreamError(t *testing.T) {
	rpcErr := errors.New("auth_service 不可达")
	stub := &stubAuthClient{verifyErr: rpcErr}
	v := NewVerifier(stub)

	if _, err := v.Verify(context.Background(), "tok", ""); err != rpcErr {
		t.Fatalf("应透传下游错误, 实际 %v", err)
	}
}

// ---------- CheckAll ----------

// TestCheckAllHealthy 全部下游正常 → healthy=true, HTTP 200。
func TestCheckAllHealthy(t *testing.T) {
	deps := &handler.Deps{
		Auth: &stubAuthClient{},
		User: &stubUserClient{},
		RAG:  &stubRAGClient{},
	}
	c := app.NewContext(0)
	CheckAll(context.Background(), c, deps)

	if c.Response.StatusCode() != consts.StatusOK {
		t.Errorf("应返回 200, 实际 %d", c.Response.StatusCode())
	}
	body := string(c.Response.Body())
	if !strings.Contains(body, `"healthy":true`) {
		t.Errorf("healthy 应为 true: %s", body)
	}
}

// TestCheckAllDownstreamDown 任一下游失败 → healthy=false, HTTP 503。
func TestCheckAllDownstreamDown(t *testing.T) {
	deps := &handler.Deps{
		Auth: &stubAuthClient{},
		User: &stubUserClient{healthErr: errors.New("pg down")},
		RAG:  &stubRAGClient{},
	}
	c := app.NewContext(0)
	CheckAll(context.Background(), c, deps)

	if c.Response.StatusCode() != consts.StatusServiceUnavailable {
		t.Errorf("应返回 503, 实际 %d", c.Response.StatusCode())
	}
	body := string(c.Response.Body())
	if !strings.Contains(body, `"healthy":false`) {
		t.Errorf("healthy 应为 false: %s", body)
	}
	if !strings.Contains(body, "pg down") {
		t.Errorf("应包含失败原因: %s", body)
	}
}
