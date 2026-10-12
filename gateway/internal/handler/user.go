package handler

import (
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	userv1 "github.com/aegis/idl/kitex_gen/userv1"
)

// ---------- 用户管理（admin scope，见 router 注册处）----------

// createUserReq 创建用户请求
type createUserReq struct {
	Username string   `json:"username"`
	Password string   `json:"password"`
	Nickname string   `json:"nickname"`
	Email    string   `json:"email"`
	Phone    string   `json:"phone"`
	Roles    []string `json:"roles"`
	Tenant   string   `json:"tenant"`
	Source   string   `json:"source"`
}

// CreateUser 创建用户（管理员）
func (d *Deps) CreateUser(c *app.RequestContext) {
	var req createUserReq
	if err := c.BindAndValidate(&req); err != nil || req.Username == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "username 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.User.CreateUser(ctx, &userv1.CreateUserReq{
		Username: req.Username,
		Password: req.Password,
		Nickname: req.Nickname,
		Email:    req.Email,
		Phone:    req.Phone,
		Roles:    req.Roles,
		Tenant:   req.Tenant,
		Source:   req.Source,
	})
	if err != nil {
		d.handleErr(ctx, c, "user.CreateUser", err)
		return
	}
	OK(c, utils.H{"uid": resp.GetUid(), "message": resp.GetMessage()})
}

// ListUsers 分页查询用户（管理员）
func (d *Deps) ListUsers(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	// keyword / status / tenant 均从 query 读取
	resp, err := d.User.ListUsers(ctx, &userv1.ListUsersReq{
		Page:    page(c),
		Keyword: queryStr(c, "keyword"),
		Tenant:  queryStr(c, "tenant"),
	})
	if err != nil {
		d.handleErr(ctx, c, "user.ListUsers", err)
		return
	}
	OK(c, utils.H{"users": resp.GetUsers(), "meta": resp.GetMeta()})
}

// GetUser 用户详情（管理员；自己查自己走 /users/me）
func (d *Deps) GetUser(c *app.RequestContext) {
	uid := paramInt64(c, "uid")
	if uid == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "uid 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.User.GetUser(ctx, &userv1.GetUserReq{Uid: uid})
	if err != nil {
		d.handleErr(ctx, c, "user.GetUser", err)
		return
	}
	OK(c, resp.GetUser())
}

// updateUserReq 更新用户请求（update_mask 指定字段）
type updateUserReq struct {
	Nickname   string   `json:"nickname"`
	Email      string   `json:"email"`
	Phone      string   `json:"phone"`
	Avatar     string   `json:"avatar"`
	Roles      []string `json:"roles"`
	UpdateMask []string `json:"update_mask"`
}

// UpdateUser 更新用户（管理员）
func (d *Deps) UpdateUser(c *app.RequestContext) {
	uid := paramInt64(c, "uid")
	if uid == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "uid 不能为空")
		return
	}

	var req updateUserReq
	if err := c.BindAndValidate(&req); err != nil {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "请求体非法")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.User.UpdateUser(ctx, &userv1.UpdateUserReq{
		Uid:        uid,
		Nickname:   req.Nickname,
		Email:      req.Email,
		Phone:      req.Phone,
		Avatar:     req.Avatar,
		Roles:      req.Roles,
		UpdateMask: req.UpdateMask,
	})
	if err != nil {
		d.handleErr(ctx, c, "user.UpdateUser", err)
		return
	}
	OK(c, resp.GetUser())
}

// deleteUserReq 删除用户请求
type deleteUserReq struct {
	Soft bool `json:"soft"`
}

// DeleteUser 删除用户（管理员；默认软删除）
func (d *Deps) DeleteUser(c *app.RequestContext) {
	uid := paramInt64(c, "uid")
	if uid == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "uid 不能为空")
		return
	}

	var req deleteUserReq
	_ = c.BindAndValidate(&req) // soft 可选

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.User.DeleteUser(ctx, &userv1.DeleteUserReq{Uid: uid, Soft: req.Soft}); err != nil {
		d.handleErr(ctx, c, "user.DeleteUser", err)
		return
	}
	OK(c, utils.H{"message": "删除成功"})
}

// ---------- 用户偏好（self-service，uid 从身份上下文取）----------

// GetPreference 获取当前用户偏好。
//
// ★ uid 只从登录身份取，不接受 query/body 覆盖 ——
//   否则任何人都能读别人的偏好。
func (d *Deps) GetPreference(c *app.RequestContext) {
	uid := idOf(c).GetUid()
	if uid == 0 {
		Error(c, consts.StatusUnauthorized, "UNAUTHORIZED", "未认证")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.User.GetPreference(ctx, &userv1.GetPreferenceReq{Uid: uid})
	if err != nil {
		d.handleErr(ctx, c, "user.GetPreference", err)
		return
	}
	OK(c, utils.H{"items": resp.GetItems()})
}

// SetPreference 设置当前用户偏好（增量合并）。
//
// 同样的硬约束：uid 只从身份取。
func (d *Deps) SetPreference(c *app.RequestContext) {
	uid := idOf(c).GetUid()
	if uid == 0 {
		Error(c, consts.StatusUnauthorized, "UNAUTHORIZED", "未认证")
		return
	}

	var req struct {
		Items map[string]string `json:"items"`
	}
	if err := c.BindAndValidate(&req); err != nil || len(req.Items) == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "items 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.User.SetPreference(ctx, &userv1.SetPreferenceReq{
		Uid:   uid,
		Items: req.Items,
	}); err != nil {
		d.handleErr(ctx, c, "user.SetPreference", err)
		return
	}
	OK(c, utils.H{"message": "已保存"})
}
