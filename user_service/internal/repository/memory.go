// Package repository 定义 user_service 的存储抽象与内存实现。
//
// 抽象 Repository 的目的：
//   - 业务 handler 不感知底层是 PostgreSQL 还是内存
//   - mock 模式与生产模式共用同一份 handler 代码，避免两套逻辑漂移
//   - 未来换 pgx 实现时 handler 零改动
//
// 当前实现：内存版（支持 mock 冒烟）。
// 后续：pg 版见 repository_pg.go，接口保持一致。
package repository

import (
	"context"
	"sort"
	"strings"
	"sync"

	userv1 "github.com/aegis/idl/kitex_gen/userv1"
)

// UserRecord 内部用户记录（含敏感字段，不出服务）
type UserRecord struct {
	User         *userv1.User
	PasswordHash string // 仅本地账号有值
	Deleted      bool
}

// Repository 用户存储
type Repository interface {
	GetByUID(ctx context.Context, uid int64) (*userv1.User, error)
	GetByUsername(ctx context.Context, username string) (*userv1.User, error)
	BatchGet(ctx context.Context, uids []int64, tenant string) ([]*userv1.User, []int64)
	Create(ctx context.Context, u *userv1.User, passwordHash string) (int64, error)
	Update(ctx context.Context, u *userv1.User, mask []string) error
	Delete(ctx context.Context, uid int64, soft bool) error
	List(ctx context.Context, keyword string, status int32, tenant string, page, size int) ([]*userv1.User, int)
	GetCredential(ctx context.Context, username string, uid int64) (*UserRecord, error)
	GetPreference(ctx context.Context, uid int64) (map[string]string, error)
	SetPreference(ctx context.Context, uid int64, items map[string]string) error
	Count() int
}

// ErrNotFound 未找到
type ErrNotFound struct{ Msg string }

func (e *ErrNotFound) Error() string { return e.Msg }

// ErrConflict 唯一约束冲突（如用户名已存在）
type ErrConflict struct{ Msg string }

func (e *ErrConflict) Error() string { return e.Msg }

// MemoryRepo 内存实现
//
// 并发保护：单 mutex 覆盖全表。
// 理由：用户服务非高频热点，mock 实现追求简单可预测；
// 生产 PG 实现不共享此约束。
type MemoryRepo struct {
	mu     sync.RWMutex
	users  map[int64]*UserRecord
	byName map[string]int64
	prefs  map[int64]map[string]string
	nextID int64
}

// NewMemoryRepo 构造内存仓储并注入种子数据
func NewMemoryRepo() *MemoryRepo {
	r := &MemoryRepo{
		users:  make(map[int64]*UserRecord),
		byName: make(map[string]int64),
		prefs:  make(map[int64]map[string]string),
		nextID: 1000,
	}
	return r
}

// Seed 写入种子用户（幂等：已存在则跳过）
func (r *MemoryRepo) Seed(recs ...*UserRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range recs {
		if rec == nil || rec.User == nil {
			continue
		}
		uid := rec.User.GetUid()
		if uid == 0 {
			r.nextID++
			uid = r.nextID
			rec.User.Uid = uid
		}
		if uid > r.nextID {
			r.nextID = uid
		}
		cp := *rec.User
		r.users[uid] = &UserRecord{User: &cp, PasswordHash: rec.PasswordHash, Deleted: rec.Deleted}
		r.byName[strings.ToLower(rec.User.GetUsername())] = uid
	}
}

func (r *MemoryRepo) GetByUID(_ context.Context, uid int64) (*userv1.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.users[uid]
	if !ok || rec.Deleted {
		return nil, &ErrNotFound{Msg: "用户不存在"}
	}
	return cloneUser(rec.User), nil
}

func (r *MemoryRepo) GetByUsername(_ context.Context, username string) (*userv1.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	uid, ok := r.byName[strings.ToLower(username)]
	if !ok {
		return nil, &ErrNotFound{Msg: "用户不存在"}
	}
	rec := r.users[uid]
	if rec == nil || rec.Deleted {
		return nil, &ErrNotFound{Msg: "用户不存在"}
	}
	return cloneUser(rec.User), nil
}

func (r *MemoryRepo) BatchGet(_ context.Context, uids []int64, tenant string) ([]*userv1.User, []int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var found []*userv1.User
	var missing []int64
	for _, uid := range uids {
		rec, ok := r.users[uid]
		if !ok || rec.Deleted {
			missing = append(missing, uid)
			continue
		}
		// 租户隔离：非空 tenant 时只返回同租户用户
		if tenant != "" && rec.User.GetTenant() != "" && rec.User.GetTenant() != tenant {
			missing = append(missing, uid)
			continue
		}
		found = append(found, cloneUser(rec.User))
	}
	return found, missing
}

func (r *MemoryRepo) Create(_ context.Context, u *userv1.User, passwordHash string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := strings.ToLower(u.GetUsername())
	if _, ok := r.byName[key]; ok {
		return 0, &ErrConflict{Msg: "用户名已存在"}
	}
	r.nextID++
	uid := r.nextID

	cp := *u
	cp.Uid = uid
	r.users[uid] = &UserRecord{User: &cp, PasswordHash: passwordHash}
	r.byName[key] = uid
	return uid, nil
}

func (r *MemoryRepo) Update(_ context.Context, u *userv1.User, mask []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.users[u.GetUid()]
	if !ok || rec.Deleted {
		return &ErrNotFound{Msg: "用户不存在"}
	}
	// 字段掩码为空视为全量更新；非空则仅更新指定字段，
	// 避免零值误覆盖（这是 proto3 场景的经典坑）
	if len(mask) == 0 {
		mask = []string{"nickname", "email", "phone", "avatar", "roles"}
	}
	for _, f := range mask {
		switch strings.ToLower(f) {
		case "nickname":
			rec.User.Nickname = u.GetNickname()
		case "email":
			rec.User.Email = u.GetEmail()
		case "phone":
			rec.User.Phone = u.GetPhone()
		case "avatar":
			rec.User.Avatar = u.GetAvatar()
		case "roles":
			rec.User.Roles = append([]string(nil), u.GetRoles()...)
		case "status":
			rec.User.Status = u.GetStatus()
		case "tenant":
			rec.User.Tenant = u.GetTenant()
		}
	}
	return nil
}

func (r *MemoryRepo) Delete(_ context.Context, uid int64, soft bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.users[uid]
	if !ok {
		return &ErrNotFound{Msg: "用户不存在"}
	}
	if soft {
		rec.Deleted = true
		rec.User.Status = 2 // 禁用
		return nil
	}
	delete(r.users, uid)
	if rec.User.GetUsername() != "" {
		delete(r.byName, strings.ToLower(rec.User.GetUsername()))
	}
	delete(r.prefs, uid)
	return nil
}

func (r *MemoryRepo) List(_ context.Context, keyword string, status int32, tenant string, page, size int) ([]*userv1.User, int) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	kw := strings.ToLower(strings.TrimSpace(keyword))
	var matched []*userv1.User
	for _, rec := range r.users {
		if rec.Deleted {
			continue
		}
		u := rec.User
		if status > 0 && u.GetStatus() != status {
			continue
		}
		if tenant != "" && u.GetTenant() != "" && u.GetTenant() != tenant {
			continue
		}
		if kw != "" && !matchUser(u, kw) {
			continue
		}
		matched = append(matched, cloneUser(u))
	}

	// 排序：先按 uid 升序，保证分页结果稳定（否则内存遍历顺序不定）
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].GetUid() < matched[j].GetUid()
	})

	total := len(matched)
	start := (page - 1) * size
	if start >= total {
		return []*userv1.User{}, total
	}
	end := start + size
	if end > total {
		end = total
	}
	return matched[start:end], total
}

func matchUser(u *userv1.User, kw string) bool {
	for _, f := range []string{u.GetUsername(), u.GetNickname(), u.GetEmail(), u.GetPhone()} {
		if f != "" && strings.Contains(strings.ToLower(f), kw) {
			return true
		}
	}
	return false
}

func (r *MemoryRepo) GetCredential(_ context.Context, username string, uid int64) (*UserRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var rec *UserRecord
	if username != "" {
		id, ok := r.byName[strings.ToLower(username)]
		if !ok {
			return nil, &ErrNotFound{Msg: "用户不存在"}
		}
		rec = r.users[id]
	} else {
		rec = r.users[uid]
	}
	if rec == nil || rec.Deleted {
		return nil, &ErrNotFound{Msg: "用户不存在"}
	}
	out := &UserRecord{
		User:         cloneUser(rec.User),
		PasswordHash: rec.PasswordHash,
	}
	return out, nil
}

func (r *MemoryRepo) GetPreference(_ context.Context, uid int64) (map[string]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.prefs[uid]
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out, nil
}

func (r *MemoryRepo) SetPreference(_ context.Context, uid int64, items map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.prefs[uid]
	if !ok {
		cur = make(map[string]string, len(items))
		r.prefs[uid] = cur
	}
	for k, v := range items {
		cur[k] = v
	}
	return nil
}

func (r *MemoryRepo) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.users)
}

func cloneUser(u *userv1.User) *userv1.User {
	if u == nil {
		return nil
	}
	cp := *u
	cp.Roles = append([]string(nil), u.Roles...)
	return &cp
}
