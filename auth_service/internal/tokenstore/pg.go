// Package tokenstore 把令牌吊销状态存到 PostgreSQL。
//
// 为什么存在：auth_service/internal/token 的吊销名单与"用户在用哪些令牌"
// 两个索引都是进程内的 map，导致
//
//	① 重启后 issued 为空 → RevokeAllOf(uid) 找不到令牌 → 登出只吊销
//	  access token，refresh token 仍可换新票（单副本就会发生）；
//	② 多副本下另一副本的 revoked 里没有该 jti → 已登出的票继续被放行。
//
// 本包只负责存取，不含缓存策略：Manager 侧的热路径与 TTL 负缓存是另一层，
// 分开的目的是"存对了"这件事可以先独立验证，不必和鉴权路径的改动混在
// 一次提交里 —— 后者出错是鉴权失效，排查代价高得多。
package tokenstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound 记录不存在
var ErrNotFound = errors.New("tokenstore: 记录不存在")

// Store 吊销状态的存取接口。
type Store interface {
	// Revoke 幂等地记下一个被吊销的 jti。重复登出同一令牌不报错。
	Revoke(ctx context.Context, uid int64, jti string, expiresAt time.Time) error
	// IsRevoked 查询 jti 是否已被吊销。
	IsRevoked(ctx context.Context, jti string) (bool, error)
	// TrackIssued 记录"该 uid 持有该 jti"，供按用户全量登出使用。
	TrackIssued(ctx context.Context, uid int64, jti string, expiresAt time.Time) error
	// ActiveJTI 列出某用户当前未过期的全部 jti。
	ActiveJTI(ctx context.Context, uid int64) ([]string, error)
	// PruneExpired 删除已过期行，返回删除条数。
	PruneExpired(ctx context.Context) (int64, error)
	Close()
}

var _ Store = (*PG)(nil)

// PG PostgreSQL 实现。
type PG struct {
	pool *pgxpool.Pool
	// closeOnce：夹具 cleanup 与用例内显式 Close 会叠加，
	// 重复关闭池不安全。
	closeOnce sync.Once
}

// NewPG 建池。DSN 为空或连不上返回 error —— 调用方据此降级为进程内名单
// （行为与引入本包之前一致），而不是让整个鉴权服务起不来。
func NewPG(ctx context.Context, pg conf.PostgresConf) (*PG, error) {
	if strings.TrimSpace(pg.DSN) == "" {
		return nil, fmt.Errorf("tokenstore: postgres DSN 未配置")
	}
	cfg, err := pgxpool.ParseConfig(pg.DSN)
	if err != nil {
		return nil, fmt.Errorf("tokenstore: 解析 DSN 失败: %w", err)
	}
	// 热路径查询，但每次只有一个索引点查；给小池避免与业务查询争抢
	cfg.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("tokenstore: 建池失败: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("tokenstore: ping 失败: %w", err)
	}
	return &PG{pool: pool}, nil
}

// Revoke 幂等写入。
//
// ON CONFLICT DO NOTHING 是必须的：登出可能被客户端重试、也可能 access 与
// refresh 成对吊销同一个 jti，第二次不应报错，否则"重复登出"会变成 500。
func (p *PG) Revoke(ctx context.Context, uid int64, jti string, expiresAt time.Time) error {
	if jti == "" {
		return fmt.Errorf("tokenstore: jti 为空")
	}
	const q = `INSERT INTO token_revocation (jti, uid, expires_at)
	           VALUES ($1, $2, $3)
	           ON CONFLICT (jti) DO NOTHING`
	if _, err := p.pool.Exec(ctx, q, jti, uid, expiresAt); err != nil {
		return fmt.Errorf("tokenstore: 写吊销失败: %w", err)
	}
	return nil
}

// IsRevoked jti 是否已被吊销。
//
// ★ 出错时返回 error 而不是"false + nil"。这个函数在鉴权路径上，
// 把它读成"未吊销"等于存储故障时**全部放行**。调用方必须显式决定
// 是拒绝请求还是退回进程内名单，不能被一个假的 false 骗过去。
func (p *PG) IsRevoked(ctx context.Context, jti string) (bool, error) {
	if jti == "" {
		return false, nil
	}
	const q = `SELECT 1 FROM token_revocation WHERE jti = $1`
	var one int
	err := p.pool.QueryRow(ctx, q, jti).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("tokenstore: 查吊销失败: %w", err)
	}
}

// TrackIssued 记录签发关系。同样幂等（同一 jti 重复登记不报错）。
func (p *PG) TrackIssued(ctx context.Context, uid int64, jti string, expiresAt time.Time) error {
	if jti == "" {
		return fmt.Errorf("tokenstore: jti 为空")
	}
	const q = `INSERT INTO issued_token (uid, jti, expires_at)
	           VALUES ($1, $2, $3)
	           ON CONFLICT (uid, jti) DO UPDATE SET expires_at = EXCLUDED.expires_at`
	if _, err := p.pool.Exec(ctx, q, uid, jti, expiresAt); err != nil {
		return fmt.Errorf("tokenstore: 写签发索引失败: %w", err)
	}
	return nil
}

// ActiveJTI 按 uid 列出未过期 jti —— 这是 RevokeAllOf 的数据来源，
// 取代原先"只记得本进程签发过"的内存 map。
func (p *PG) ActiveJTI(ctx context.Context, uid int64) ([]string, error) {
	const q = `SELECT jti FROM issued_token
	           WHERE uid = $1 AND expires_at > now()`
	rows, err := p.pool.Query(ctx, q, uid)
	if err != nil {
		return nil, fmt.Errorf("tokenstore: 查在用令牌失败: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var jti string
		if err := rows.Scan(&jti); err != nil {
			return nil, fmt.Errorf("tokenstore: 扫描失败: %w", err)
		}
		out = append(out, jti)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tokenstore: 遍历失败: %w", err)
	}
	return out, nil
}

// PruneExpired 删除两张表里已过期的行。
//
// 为什么必须有人调它：吊销名单只增不减的话，refresh token 有 7 天有效期，
// 表会随活跃登录无限增长。而行一旦 expires_at 过去就没有存在意义 ——
// JWT 校验那一层已经先拒了过期票，继续留着只会让索引白长。
// 调用方应挂一个定时任务（低频即可，例如每小时）。
func (p *PG) PruneExpired(ctx context.Context) (int64, error) {
	const q = `WITH d1 AS (
	               DELETE FROM token_revocation WHERE expires_at <= now() RETURNING 1
	             ), d2 AS (
	               DELETE FROM issued_token      WHERE expires_at <= now() RETURNING 1
	             )
	             SELECT (SELECT count(*) FROM d1) + (SELECT count(*) FROM d2)`
	var n int64
	if err := p.pool.QueryRow(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("tokenstore: 清理失败: %w", err)
	}
	if n > 0 {
		logger.Info(ctx, "tokenstore: 已清理过期令牌记录", "rows", n)
	}
	return n, nil
}

// Close 释放连接池。重复调用安全。
func (p *PG) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		if p.pool != nil {
			p.pool.Close()
		}
	})
}
