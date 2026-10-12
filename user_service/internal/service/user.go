// Package service 实现 user_service 的业务 handler。
package service

import (
	"context"
	"errors"
	"time"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	userv1 "github.com/aegis/idl/kitex_gen/userv1"

	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/pagination"
	"github.com/aegis/pkg/passwd"
	"github.com/aegis/user_service/internal/repository"
)

// UserService 实现 userv1.UserService
type UserService struct {
	repo repository.Repository
	mock *mockx.FaultInjector

	startedAt time.Time
	version   string
	gitCommit string
}

// New 构造 handler
func New(repo repository.Repository, mock *mockx.FaultInjector) *UserService {
	return &UserService{
		repo:      repo,
		mock:      mock,
		startedAt: time.Now(),
		version:   "dev",
	}
}

// 编译期断言：确保实现了 Kitex 生成的接口
var _ userv1.UserService = (*UserService)(nil)

func nowSec() int64 { return time.Now().Unix() }

// ---------- 健康检查 ----------

func (s *UserService) Health(ctx context.Context, _ *commonv1.HealthReq) (*commonv1.HealthResp, error) {
	if s.mock.ShouldFail("health") {
		return &commonv1.HealthResp{Healthy: false, Service: "user_service"}, nil
	}
	return &commonv1.HealthResp{
		Healthy:   true,
		Service:   "user_service",
		Version:   s.version,
		GitCommit: s.gitCommit,
		UptimeSec: uint64(time.Since(s.startedAt).Seconds()),
		MockMode:  s.mock.Enabled(),
	}, nil
}

// ---------- 查询 ----------

func (s *UserService) GetUser(ctx context.Context, req *userv1.GetUserReq) (*userv1.GetUserResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}

	var (
		u   *userv1.User
		err error
	)
	switch {
	case req.GetUid() != 0:
		u, err = s.repo.GetByUID(ctx, req.GetUid())
	case req.GetUsername() != "":
		u, err = s.repo.GetByUsername(ctx, req.GetUsername())
	default:
		return nil, errno.New(errno.ErrInvalidParam)
	}
	if err != nil {
		if isNotFound(err) {
			return nil, errno.Wrap(errno.ErrUserNotFound, err)
		}
		return nil, err
	}
	return &userv1.GetUserResp{Success: true, User: u}, nil
}

func (s *UserService) BatchGetUsers(ctx context.Context, req *userv1.BatchGetUsersReq) (*userv1.BatchGetUsersResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	users, missing := s.repo.BatchGet(ctx, req.GetUids(), req.GetTenant())
	return &userv1.BatchGetUsersResp{Users: users, MissingUids: missing}, nil
}

func (s *UserService) ListUsers(ctx context.Context, req *userv1.ListUsersReq) (*userv1.ListUsersResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	page, size := pagination.Normalize(req.GetPage())
	users, total := s.repo.List(ctx, req.GetKeyword(), req.GetStatus(), req.GetTenant(), page, size)
	return &userv1.ListUsersResp{
		Users: users,
		Meta:  pagination.Meta(req.GetPage(), total),
	}, nil
}

// ---------- 写入 ----------

func (s *UserService) CreateUser(ctx context.Context, req *userv1.CreateUserReq) (*userv1.CreateUserResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if req.GetUsername() == "" {
		return nil, errno.New(errno.ErrInvalidParam)
	}

	source := req.GetSource()
	if source == "" {
		source = "local"
	}

	u := &userv1.User{
		Username: req.GetUsername(),
		Nickname: firstNonEmpty(req.GetNickname(), req.GetUsername()),
		Email:    req.GetEmail(),
		Phone:    req.GetPhone(),
		Roles:    req.GetRoles(),
		Tenant:   firstNonEmpty(req.GetTenant(), "default"),
		Status:   1,
		Source:   source,
	}
	if len(u.Roles) == 0 {
		u.Roles = []string{"user"}
	}
	now := nowSec()
	u.CreatedAt = now
	u.UpdatedAt = now

	// ★ 口令以哈希形式存储，明文绝不落库。
	//   auth_service 取到 hash 后用 pkg/passwd 校验（同一份实现，不会漂移）。
	hash, err := passwd.Hash(req.GetPassword())
	if err != nil {
		// crypto/rand 熵源异常：让创建请求 fail，绝不降级到弱随机。
		logger.Error(ctx, "生成口令哈希失败（系统熵源异常）", "err", err)
		return nil, err
	}

	uid, err := s.repo.Create(ctx, u, hash)
	if err != nil {
		var conflict *repository.ErrConflict
		if errors.As(err, &conflict) {
			return &userv1.CreateUserResp{
				Success: false,
				Message: conflict.Msg,
			}, nil
		}
		return nil, err
	}

	logger.Info(ctx, "user created",
		"uid", uid, "username", req.GetUsername(), "source", source)

	return &userv1.CreateUserResp{Success: true, Uid: uid}, nil
}

func (s *UserService) UpdateUser(ctx context.Context, req *userv1.UpdateUserReq) (*userv1.UpdateUserResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	uid := req.GetUid()
	if uid == 0 {
		return nil, errno.New(errno.ErrInvalidParam)
	}

	// 注意：Kitex 生成代码把 UpdateUserReq 的字段平铺在请求结构体上，
	// 没有嵌套 user message，因此直接用 req 本身作为更新载体。
	if err := s.repo.Update(ctx, reqToUser(req), req.GetUpdateMask()); err != nil {
		if isNotFound(err) {
			return nil, errno.Wrap(errno.ErrUserNotFound, err)
		}
		return nil, err
	}
	updated, err := s.repo.GetByUID(ctx, uid)
	if err != nil {
		return nil, errno.Wrap(errno.ErrUserNotFound, err)
	}
	logger.Info(ctx, "user updated", "uid", uid, "mask", req.GetUpdateMask())
	return &userv1.UpdateUserResp{Success: true, User: updated}, nil
}

// reqToUser 把扁平的 UpdateUserReq 转为 User 交给仓储层
func reqToUser(req *userv1.UpdateUserReq) *userv1.User {
	return &userv1.User{
		Uid:      req.GetUid(),
		Nickname: req.GetNickname(),
		Email:    req.GetEmail(),
		Phone:    req.GetPhone(),
		Avatar:   req.GetAvatar(),
		Roles:    req.GetRoles(),
	}
}

func (s *UserService) DeleteUser(ctx context.Context, req *userv1.DeleteUserReq) (*commonv1.OpResult, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if req.GetUid() == 0 {
		return nil, errno.New(errno.ErrInvalidParam)
	}
	// proto3 无法区分 soft=false 与"未设置"，因此默认软删（更安全，可恢复）
	soft := req.GetSoft()
	if err := s.repo.Delete(ctx, req.GetUid(), soft); err != nil {
		if isNotFound(err) {
			return nil, errno.Wrap(errno.ErrUserNotFound, err)
		}
		return nil, err
	}
	logger.Info(ctx, "user deleted", "uid", req.GetUid(), "soft", soft)
	return &commonv1.OpResult{Success: true, Message: "删除成功"}, nil
}

// ---------- 认证域专用 ----------

func (s *UserService) GetUserCredential(ctx context.Context, req *userv1.GetUserCredentialReq) (*userv1.GetUserCredentialResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if req.GetUsername() == "" && req.GetUid() == 0 {
		return nil, errno.New(errno.ErrInvalidParam)
	}
	rec, err := s.repo.GetCredential(ctx, req.GetUsername(), req.GetUid())
	if err != nil {
		if isNotFound(err) {
			// ★ 不返回错误而是 found=false：
			// 认证失败时"用户不存在"与"密码错误"应返回同样的错误，
			// 否则可被用来枚举用户名。这里让 auth_service 自行归一化。
			return &userv1.GetUserCredentialResp{Found: false}, nil
		}
		return nil, err
	}
	return &userv1.GetUserCredentialResp{
		Found:        true,
		Uid:          rec.User.GetUid(),
		Username:     rec.User.GetUsername(),
		PasswordHash: rec.PasswordHash,
		Status:       rec.User.GetStatus(),
		Roles:        rec.User.GetRoles(),
		Tenant:       rec.User.GetTenant(),
	}, nil
}

// ---------- 偏好 ----------

func (s *UserService) GetPreference(ctx context.Context, req *userv1.GetPreferenceReq) (*userv1.GetPreferenceResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	items, err := s.repo.GetPreference(ctx, req.GetUid())
	if err != nil {
		return nil, err
	}
	return &userv1.GetPreferenceResp{Items: items}, nil
}

func (s *UserService) SetPreference(ctx context.Context, req *userv1.SetPreferenceReq) (*commonv1.OpResult, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if err := s.repo.SetPreference(ctx, req.GetUid(), req.GetItems()); err != nil {
		return nil, err
	}
	return &commonv1.OpResult{Success: true, Message: "已保存"}, nil
}

// ---------- 工具 ----------

func isNotFound(err error) bool {
	var nf *repository.ErrNotFound
	return errors.As(err, &nf)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
