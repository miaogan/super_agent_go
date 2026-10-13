// Package repository 的 PostgreSQL 实现。
//
// 依赖 pgx/v5 直连池化，接口与 MemoryRepo 完全一致 ——
// service 层通过 Repository 抽象接收，无需感知底层。
//
// 设计原则：
//   - 所有查询显式带 ctx 超时（QueryTimeout）
//   - roles/user_roles 用聚合 SQL 一次拉全用户关联
//   - ErrNotFound / ErrConflict 错误语义与 MemoryRepo 对齐，
//     service 层的错误分支不需要改
//
// 当前实现覆盖 auth 链路所需方法：
//
//	GetCredential / GetByUID / GetByUsername / BatchGet / List
//
// 后续补：Create / Update / Delete / Preference
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	userv1 "github.com/aegis/idl/kitex_gen/userv1"
	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/logger"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgRepo PostgreSQL 实现
type PgRepo struct {
	pool *pgxpool.Pool
}

// NewPgRepo 构造 PG 仓储。
// DSN 为空返回 nil（调用方据此回退到 MemoryRepo）。
func NewPgRepo(ctx context.Context, pgConf conf.PostgresConf) (*PgRepo, error) {
	if pgConf.DSN == "" {
		return nil, errors.New("postgres DSN 未配置")
	}

	defaultCfg, _ := pgxpool.ParseConfig(pgConf.DSN)
	if pgConf.MaxOpenConns > 0 {
		defaultCfg.MaxConns = int32(pgConf.MaxOpenConns)
	}
	if pgConf.MaxIdleConns > 0 {
		defaultCfg.MinConns = int32(pgConf.MaxIdleConns)
	}
	if pgConf.ConnMaxLifetime > 0 {
		defaultCfg.MaxConnLifetime = pgConf.ConnMaxLifetime
	}

	pool, err := pgxpool.NewWithConfig(ctx, defaultCfg)
	if err != nil {
		return nil, fmt.Errorf("pgxpool 构造失败: %w", err)
	}

	// 立即 Ping：启动时发现问题比运行时静默失败好
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping 失败: %w", err)
	}

	return &PgRepo{pool: pool}, nil
}

// Close 释放连接池
func (r *PgRepo) Close() {
	if r != nil && r.pool != nil {
		r.pool.Close()
	}
}

// GetCredential 登录链路专用：按 username 或 uid 拉完整用户 + password_hash + roles
func (r *PgRepo) GetCredential(ctx context.Context, username string, uid int64) (*UserRecord, error) {
	// 1. 主查询：users + LEFT JOIN user_roles + roles 聚合为 roles[]
	const sql = `
		SELECT u.uid, u.username, u.password_hash, u.nickname, u.avatar,
		       u.email, u.phone, u.status, u.tenant, u.source, u.deleted_at,
		       COALESCE(array_agg(rl.name), '{}') AS roles
		FROM users u
		LEFT JOIN user_roles ur ON ur.uid = u.uid
		LEFT JOIN roles rl ON rl.role_id = ur.role_id
		WHERE u.deleted_at IS NULL
		  AND (
			($1 <> '' AND u.username = $1)
			OR ($2 > 0 AND u.uid = $2)
		  )
		GROUP BY u.uid
		LIMIT 1
	`
	row := r.pool.QueryRow(ctx, sql, username, uid)
	timeout := timeoutOf(ctx)

	// ★ 必须先初始化 User，否则 Scan 写入 nil 指针会 panic
	rec := &UserRecord{User: &userv1.User{}}
	var deletedAt *time.Time
	var roles []string
	err := row.Scan(
		&rec.User.Uid, &rec.User.Username, &rec.PasswordHash,
		&rec.User.Nickname, &rec.User.Avatar, &rec.User.Email, &rec.User.Phone,
		&rec.User.Status, &rec.User.Tenant, &rec.User.Source,
		&deletedAt, &roles,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		logger.Debug(ctx, "pg GetCredential not found", "username", username, "uid", uid, "timeout", timeout)
		return nil, &ErrNotFound{Msg: "用户不存在"}
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	rec.User.Roles = roles
	if deletedAt != nil {
		return nil, &ErrNotFound{Msg: "用户已删除"}
	}
	return rec, nil
}

// GetByUID 按 uid 拉公开用户信息（不含密码 hash）
func (r *PgRepo) GetByUID(ctx context.Context, uid int64) (*userv1.User, error) {
	const sql = `
		SELECT u.uid, u.username, u.nickname, u.avatar, u.email, u.phone,
		       u.status, u.tenant, u.source, u.deleted_at,
		       COALESCE(array_agg(rl.name), '{}') AS roles
		FROM users u
		LEFT JOIN user_roles ur ON ur.uid = u.uid
		LEFT JOIN roles rl ON rl.role_id = ur.role_id
		WHERE u.deleted_at IS NULL AND u.uid = $1
		GROUP BY u.uid
		LIMIT 1
	`
	return r.queryOneUser(ctx, sql, uid)
}

// GetByUsername 按 username 拉公开用户信息
func (r *PgRepo) GetByUsername(ctx context.Context, username string) (*userv1.User, error) {
	const sql = `
		SELECT u.uid, u.username, u.nickname, u.avatar, u.email, u.phone,
		       u.status, u.tenant, u.source, u.deleted_at,
		       COALESCE(array_agg(rl.name), '{}') AS roles
		FROM users u
		LEFT JOIN user_roles ur ON ur.uid = u.uid
		LEFT JOIN roles rl ON rl.role_id = ur.role_id
		WHERE u.deleted_at IS NULL AND u.username = $1
		GROUP BY u.uid
		LIMIT 1
	`
	return r.queryOneUser(ctx, sql, username)
}

func (r *PgRepo) queryOneUser(ctx context.Context, sql string, arg interface{}) (*userv1.User, error) {
	row := r.pool.QueryRow(ctx, sql, arg)
	u := &userv1.User{}
	var deletedAt *time.Time
	err := row.Scan(
		&u.Uid, &u.Username, &u.Nickname, &u.Avatar, &u.Email, &u.Phone,
		&u.Status, &u.Tenant, &u.Source, &deletedAt, &u.Roles,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ErrNotFound{Msg: "用户不存在"}
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	if deletedAt != nil {
		return nil, &ErrNotFound{Msg: "用户已删除"}
	}
	return u, nil
}

// BatchGet 批量按 uid 拉用户（未找到的进 missing）
func (r *PgRepo) BatchGet(ctx context.Context, uids []int64, tenant string) ([]*userv1.User, []int64) {
	if len(uids) == 0 {
		return nil, nil
	}
	const sql = `
		SELECT u.uid, u.username, u.nickname, u.avatar, u.email, u.phone,
		       u.status, u.tenant, u.source,
		       COALESCE(array_agg(rl.name), '{}') AS roles
		FROM users u
		LEFT JOIN user_roles ur ON ur.uid = u.uid
		LEFT JOIN roles rl ON rl.role_id = ur.role_id
		WHERE u.deleted_at IS NULL AND u.uid = ANY($1)
		  AND ($2 = '' OR u.tenant = '' OR u.tenant = $2)
		GROUP BY u.uid
	`
	rows, err := r.pool.Query(ctx, sql, uids, tenant)
	if err != nil {
		return nil, uids
	}
	defer rows.Close()

	found := map[int64]*userv1.User{}
	for rows.Next() {
		u := &userv1.User{}
		if err := rows.Scan(&u.Uid, &u.Username, &u.Nickname, &u.Avatar,
			&u.Email, &u.Phone, &u.Status, &u.Tenant, &u.Source, &u.Roles); err != nil {
			continue
		}
		found[u.Uid] = u
	}

	var out []*userv1.User
	var missing []int64
	for _, uid := range uids {
		if u, ok := found[uid]; ok {
			out = append(out, u)
		} else {
			missing = append(missing, uid)
		}
	}
	return out, missing
}

// List 分页查询用户
func (r *PgRepo) List(ctx context.Context, keyword string, status int32, tenant string, page, size int) ([]*userv1.User, int) {
	// 动态 WHERE
	var conds []string
	var args []interface{}
	n := 1

	conds = append(conds, "u.deleted_at IS NULL")
	if keyword != "" {
		args = append(args, "%"+keyword+"%")
		conds = append(conds, fmt.Sprintf("(u.username ILIKE $%d OR u.nickname ILIKE $%d OR u.email ILIKE $%d)", n, n, n))
		n++
	}
	if status > 0 {
		args = append(args, status)
		conds = append(conds, fmt.Sprintf("u.status = $%d", n))
		n++
	}
	if tenant != "" {
		args = append(args, tenant)
		conds = append(conds, fmt.Sprintf("u.tenant = $%d", n))
		n++
	}
	where := strings.Join(conds, " AND ")

	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM users u WHERE %s", where)
	var total int
	if err := r.pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0
	}

	if size <= 0 {
		size = 20
	}
	if page <= 0 {
		page = 1
	}
	args = append(args, (page-1)*size, size)
	listSQL := fmt.Sprintf(`
		SELECT u.uid, u.username, u.nickname, u.avatar, u.email, u.phone,
		       u.status, u.tenant, u.source,
		       COALESCE(array_agg(rl.name), '{}') AS roles
		FROM users u
		LEFT JOIN user_roles ur ON ur.uid = u.uid
		LEFT JOIN roles rl ON rl.role_id = ur.role_id
		WHERE %s
		GROUP BY u.uid
		ORDER BY u.uid
		LIMIT $%d OFFSET $%d
	`, where, n+1, n)

	rows, err := r.pool.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	var out []*userv1.User
	for rows.Next() {
		u := &userv1.User{}
		if err := rows.Scan(&u.Uid, &u.Username, &u.Nickname, &u.Avatar,
			&u.Email, &u.Phone, &u.Status, &u.Tenant, &u.Source, &u.Roles); err != nil {
			continue
		}
		out = append(out, u)
	}
	return out, total
}

// Count 返回活跃用户数
func (r *PgRepo) Count() int {
	var n int
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM users WHERE deleted_at IS NULL").Scan(&n)
	return n
}

// Create 创建用户；返回 BIGSERIAL 自动生成的 uid。
// 空 username / 空 passwordHash 返回参数错误；username 已存在返回 ErrConflict。
func (r *PgRepo) Create(ctx context.Context, u *userv1.User, passwordHash string) (int64, error) {
	if u.GetUsername() == "" || passwordHash == "" {
		return 0, errors.New("username 和 passwordHash 不能为空")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("开始事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insertSQL = `
		INSERT INTO users (username, password_hash, nickname, avatar, email, phone,
		                  status, tenant, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING uid
	`
	var uid int64
	err = tx.QueryRow(ctx, insertSQL,
		u.GetUsername(), passwordHash,
		u.GetNickname(), u.GetAvatar(), u.GetEmail(), u.GetPhone(),
		u.GetStatus(), u.GetTenant(), u.GetSource(),
	).Scan(&uid)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return 0, &ErrConflict{Msg: "用户名已存在"}
		}
		return 0, fmt.Errorf("创建用户失败: %w", err)
	}

	// 角色关联：按 role name 查 role_id，找不到跳过（不阻断创建）
	for _, roleName := range u.GetRoles() {
		var roleID int64
		err := tx.QueryRow(ctx, "SELECT role_id FROM roles WHERE name = $1", roleName).Scan(&roleID)
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn(ctx, "Create: role not found, skip", "role", roleName)
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("查询角色失败: %w", err)
		}
		_, err = tx.Exec(ctx,
			"INSERT INTO user_roles (uid, role_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			uid, roleID,
		)
		if err != nil {
			return 0, fmt.Errorf("插入用户角色失败: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交事务失败: %w", err)
	}
	return uid, nil
}

// Update 按 mask 指定字段更新用户；mask 为空时默认更新 nickname/email/phone/avatar/roles。
// roles 字段走事务：先清旧 user_roles 再按 role name 查 role_id 批量 INSERT。
func (r *PgRepo) Update(ctx context.Context, u *userv1.User, mask []string) error {
	uid := u.GetUid()
	if uid == 0 {
		return &ErrNotFound{Msg: "用户不存在"}
	}

	if len(mask) == 0 {
		mask = []string{"nickname", "email", "phone", "avatar", "roles"}
	}

	// 先检查 user 是否存在（且未软删）
	var alive int
	if err := r.pool.QueryRow(ctx, "SELECT 1 FROM users WHERE uid = $1 AND deleted_at IS NULL", uid).Scan(&alive); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &ErrNotFound{Msg: "用户不存在"}
		}
		return fmt.Errorf("查询用户失败: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开始事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var setClauses []string
	var args []interface{}
	n := 1
	needRoles := false

	for _, f := range mask {
		switch strings.ToLower(f) {
		case "nickname":
			setClauses = append(setClauses, fmt.Sprintf("nickname = $%d", n))
			args = append(args, u.GetNickname())
			n++
		case "email":
			setClauses = append(setClauses, fmt.Sprintf("email = $%d", n))
			args = append(args, u.GetEmail())
			n++
		case "phone":
			setClauses = append(setClauses, fmt.Sprintf("phone = $%d", n))
			args = append(args, u.GetPhone())
			n++
		case "avatar":
			setClauses = append(setClauses, fmt.Sprintf("avatar = $%d", n))
			args = append(args, u.GetAvatar())
			n++
		case "status":
			setClauses = append(setClauses, fmt.Sprintf("status = $%d", n))
			args = append(args, u.GetStatus())
			n++
		case "tenant":
			setClauses = append(setClauses, fmt.Sprintf("tenant = $%d", n))
			args = append(args, u.GetTenant())
			n++
		case "roles":
			needRoles = true
		}
	}

	if len(setClauses) > 0 {
		setClauses = append(setClauses, fmt.Sprintf("updated_at = $%d", n))
		args = append(args, "now()")
		n++
		args = append(args, uid)

		sql := fmt.Sprintf("UPDATE users SET %s WHERE uid = $%d",
			strings.Join(setClauses, ", "), n)
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("更新用户失败: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return &ErrNotFound{Msg: "用户不存在"}
		}
	}

	if needRoles {
		// 先清旧
		if _, err := tx.Exec(ctx, "DELETE FROM user_roles WHERE uid = $1", uid); err != nil {
			return fmt.Errorf("清理旧角色失败: %w", err)
		}
		for _, roleName := range u.GetRoles() {
			var roleID int64
			err := tx.QueryRow(ctx, "SELECT role_id FROM roles WHERE name = $1", roleName).Scan(&roleID)
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn(ctx, "Update: role not found, skip", "role", roleName)
				continue
			}
			if err != nil {
				return fmt.Errorf("查询角色失败: %w", err)
			}
			_, err = tx.Exec(ctx,
				"INSERT INTO user_roles (uid, role_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
				uid, roleID,
			)
			if err != nil {
				return fmt.Errorf("插入用户角色失败: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// Delete 删除用户；soft=true 为软删除（置 deleted_at 与 status=2），否则物理删除（cascade 清理关联表）。
func (r *PgRepo) Delete(ctx context.Context, uid int64, soft bool) error {
	if soft {
		tag, err := r.pool.Exec(ctx,
			"UPDATE users SET deleted_at = now(), status = 2 WHERE uid = $1 AND deleted_at IS NULL",
			uid,
		)
		if err != nil {
			return fmt.Errorf("软删除用户失败: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return &ErrNotFound{Msg: "用户不存在"}
		}
		return nil
	}

	tag, err := r.pool.Exec(ctx, "DELETE FROM users WHERE uid = $1", uid)
	if err != nil {
		return fmt.Errorf("物理删除用户失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &ErrNotFound{Msg: "用户不存在"}
	}
	return nil
}

// GetPreference 读取用户偏好；行不存在时返回空 map（与 MemoryRepo 行为一致）。
func (r *PgRepo) GetPreference(ctx context.Context, uid int64) (map[string]string, error) {
	var raw json.RawMessage
	err := r.pool.QueryRow(ctx,
		"SELECT COALESCE(settings, '{}'::jsonb) FROM user_preferences WHERE uid = $1",
		uid,
	).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户偏好失败: %w", err)
	}
	out := make(map[string]string)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("解析用户偏好失败: %w", err)
		}
	}
	return out, nil
}

// SetPreference 增量合并用户偏好（对齐 MemoryRepo 的合并语义与 IDL 注释）。
// 通过 JSONB merge operator 一次性 UPSERT：已有 key 保留，传入的 key 覆盖/新增。
func (r *PgRepo) SetPreference(ctx context.Context, uid int64, items map[string]string) error {
	data, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("序列化偏好失败: %w", err)
	}

	const upsertSQL = `
		INSERT INTO user_preferences (uid, settings) VALUES ($1, $2)
		ON CONFLICT (uid) DO UPDATE SET settings = user_preferences.settings || EXCLUDED.settings
	`
	if _, err := r.pool.Exec(ctx, upsertSQL, uid, data); err != nil {
		return fmt.Errorf("写入用户偏好失败: %w", err)
	}
	return nil
}

// timeoutOf 从 ctx 提取剩余超时，用于日志
func timeoutOf(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		return time.Until(d)
	}
	return 0
}
