package repository

import (
	"context"
	"testing"

	userv1 "github.com/aegis/idl/kitex_gen/userv1"
)

// TestCreateGetRoundTrip 创建后可按 uid / username 读取。
func TestCreateGetRoundTrip(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()

	uid, err := r.Create(ctx, &userv1.User{Username: "alice", Tenant: "t1"}, "hash")
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	u, err := r.GetByUID(ctx, uid)
	if err != nil {
		t.Fatalf("GetByUID 失败: %v", err)
	}
	if u.GetUsername() != "alice" || u.GetTenant() != "t1" {
		t.Errorf("字段不符: %+v", u)
	}
	byName, err := r.GetByUsername(ctx, "ALICE") // 大小写不敏感
	if err != nil || byName.GetUid() != uid {
		t.Errorf("按用户名读取失败: %v", err)
	}
	// 重复用户名冲突
	if _, err := r.Create(ctx, &userv1.User{Username: "alice"}, ""); err == nil {
		t.Error("重复用户名应 ErrConflict")
	}
}

// TestUpdateMask 掩码更新只动指定字段。
func TestUpdateMask(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	uid, _ := r.Create(ctx, &userv1.User{Username: "bob", Email: "old@x.io"}, "")

	if err := r.Update(ctx, &userv1.User{Uid: uid, Email: "new@x.io", Nickname: "b"}, []string{"email"}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	u, _ := r.GetByUID(ctx, uid)
	if u.GetEmail() != "new@x.io" {
		t.Errorf("email 应更新: %q", u.GetEmail())
	}
	if u.GetNickname() != "" {
		t.Errorf("未在掩码中的 nickname 不应被修改: %q", u.GetNickname())
	}
	// 不存在
	if err := r.Update(ctx, &userv1.User{Uid: 99999}, nil); err == nil {
		t.Error("更新不存在用户应报错")
	}
}

// TestDeleteSoftHard 软删保留记录、硬删彻底移除。
func TestDeleteSoftHard(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()

	uid1, _ := r.Create(ctx, &userv1.User{Username: "soft"}, "")
	if err := r.Delete(ctx, uid1, true); err != nil {
		t.Fatalf("软删失败: %v", err)
	}
	if _, err := r.GetByUID(ctx, uid1); err == nil {
		t.Error("软删后不可查询")
	}
	if r.Count() != 1 {
		t.Errorf("软删应保留记录, Count=%d", r.Count())
	}

	uid2, _ := r.Create(ctx, &userv1.User{Username: "hard"}, "")
	if err := r.Delete(ctx, uid2, false); err != nil {
		t.Fatalf("硬删失败: %v", err)
	}
	if _, err := r.GetByUID(ctx, uid2); err == nil {
		t.Error("硬删后不可查询")
	}
	if r.Count() != 1 {
		t.Errorf("硬删应移除记录, Count=%d", r.Count())
	}
}

// TestBatchGetTenantIsolation 租户隔离：跨租户用户进 missing。
func TestBatchGetTenantIsolation(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	id1, _ := r.Create(ctx, &userv1.User{Username: "a", Tenant: "t1"}, "")
	id2, _ := r.Create(ctx, &userv1.User{Username: "b", Tenant: "t2"}, "")

	found, missing := r.BatchGet(ctx, []int64{id1, id2}, "t1")
	if len(found) != 1 || found[0].GetUid() != id1 {
		t.Errorf("应只返回同租户用户: %+v", found)
	}
	if len(missing) != 1 || missing[0] != id2 {
		t.Errorf("跨租户应进 missing: %v", missing)
	}
}

// TestListFilterSort 列表过滤、排序与分页稳定性。
func TestListFilterSort(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		r.Create(ctx, &userv1.User{Username: "u" + string(rune('0'+i)), Tenant: "t1"}, "")
	}
	r.Create(ctx, &userv1.User{Username: "x", Tenant: "t2"}, "")

	users, total := r.List(ctx, "", 0, "t1", 1, 2)
	if total != 3 || len(users) != 2 {
		t.Errorf("分页不符: n=%d total=%d", len(users), total)
	}
	// 按 uid 升序稳定
	if users[0].GetUid() > users[1].GetUid() {
		t.Error("应按 uid 升序")
	}
	// 关键词过滤
	kw, _ := r.List(ctx, "u1", 0, "", 1, 20)
	if len(kw) != 1 || kw[0].GetUsername() != "u1" {
		t.Errorf("关键词过滤不符: %+v", kw)
	}
}

// TestCredentialNotFound 凭据不存在返回 ErrNotFound。
func TestCredentialNotFound(t *testing.T) {
	r := NewMemoryRepo()
	_, err := r.GetCredential(context.Background(), "ghost", 0)
	if err == nil {
		t.Error("不存在应返回错误")
	}
}

// TestPreference 偏好读写与隔离。
func TestPreference(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	if err := r.SetPreference(ctx, 1, map[string]string{"a": "1"}); err != nil {
		t.Fatalf("SetPreference 失败: %v", err)
	}
	got, _ := r.GetPreference(ctx, 1)
	if got["a"] != "1" {
		t.Errorf("偏好不符: %v", got)
	}
	// 用户间隔离
	other, _ := r.GetPreference(ctx, 2)
	if _, ok := other["a"]; ok {
		t.Error("偏好应按用户隔离")
	}
}

// TestSeedIdempotent 种子注入幂等。
func TestSeedIdempotent(t *testing.T) {
	r := NewMemoryRepo()
	r.Seed(&UserRecord{User: &userv1.User{Uid: 1001, Username: "admin"}, PasswordHash: "h"})
	r.Seed(&UserRecord{User: &userv1.User{Uid: 1001, Username: "admin"}, PasswordHash: "h"})

	if _, err := r.GetByUID(context.Background(), 1001); err != nil {
		t.Fatalf("种子用户应存在: %v", err)
	}
	if r.Count() != 1 {
		t.Errorf("种子应幂等, Count=%d", r.Count())
	}
}
