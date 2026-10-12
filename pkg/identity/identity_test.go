package identity

import (
	"context"
	"testing"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
)

// mkIdentity 构造测试身份
func mkIdentity(uid int64, scopes ...string) *commonv1.Identity {
	return &commonv1.Identity{Uid: uid, Tenant: "t1", Scopes: scopes}
}

// TestWithFromRoundTrip With 写入后 From 能读回。
func TestWithFromRoundTrip(t *testing.T) {
	ctx := With(context.Background(), mkIdentity(42, "user", "admin"))
	id := From(ctx)
	if id.GetUid() != 42 || id.GetTenant() != "t1" {
		t.Errorf("身份读回不符: %+v", id)
	}
	// 未写入的 ctx 返回匿名身份而非 nil
	if got := From(context.Background()); got.GetUid() != 0 {
		t.Errorf("空 ctx 应返回匿名身份, 实际 %+v", got)
	}
	// nil 身份不应 panic，且不污染 ctx
	ctx2 := With(context.Background(), nil)
	if From(ctx2).GetUid() != 0 {
		t.Error("With(nil) 后 From 应返回匿名身份")
	}
	// nil ctx
	if got := From(nil); got.GetUid() != 0 {
		t.Errorf("From(nil) 应返回匿名身份, 实际 %+v", got)
	}
}

// TestUidTenantAnonymous 便捷读取函数。
func TestUidTenantAnonymous(t *testing.T) {
	ctx := With(context.Background(), mkIdentity(7, "user"))
	if Uid(ctx) != 7 {
		t.Errorf("Uid = %d, 期望 7", Uid(ctx))
	}
	if Tenant(ctx) != "t1" {
		t.Errorf("Tenant = %q, 期望 t1", Tenant(ctx))
	}
	if Anonymous(ctx) {
		t.Error("已登录身份不应判为匿名")
	}
	if !Anonymous(context.Background()) {
		t.Error("空 ctx 应判为匿名")
	}
}

// TestIsAdmin admin 或 * scope 才视为管理员。
func TestIsAdmin(t *testing.T) {
	cases := []struct {
		name   string
		scopes []string
		want   bool
	}{
		{"user only", []string{"user"}, false},
		{"admin", []string{"user", "admin"}, true},
		{"wildcard", []string{"*"}, true},
		{"empty", nil, false},
	}
	for _, c := range cases {
		ctx := With(context.Background(), mkIdentity(1, c.scopes...))
		if got := IsAdmin(ctx); got != c.want {
			t.Errorf("%s: IsAdmin = %v, 期望 %v", c.name, got, c.want)
		}
	}
	// 空 ctx
	if IsAdmin(context.Background()) {
		t.Error("空 ctx 不应为 admin")
	}
}
