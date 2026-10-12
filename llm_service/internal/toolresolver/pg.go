// Package toolresolver — llm_service 侧的工具定义解析。
//
// LLMService 把工具名列表透传到 Provider 前，需要先补全完整 JSON Schema。
// 本包提供 PG 只读实现：查 tools 表 → provider.Tool。
//
// 连接池刻意做得很小（MaxConns=2）：工具解析不是热点，
// 一次 Chat 最多一次查询，且很多 Chat 根本不调工具。
package toolresolver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/provider"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGResolver 从 PostgreSQL tools 表解析工具定义。
// 只读连接池，专为工具查询设计。
type PGResolver struct {
	pool *pgxpool.Pool
}

// NewPG 构造 PGResolver。
//
// DSN 为空返回 nil，调用方据此禁用 ToolResolver（降级为忽略 req.Tools）。
// 连接失败也返回 nil —— 让 llm_service 继续启动，只是不支持工具调用。
func NewPG(ctx context.Context, pgConf conf.PostgresConf) (*PGResolver, error) {
	if pgConf.DSN == "" {
		return nil, errors.New("postgres DSN 未配置")
	}

	cfg, _ := pgxpool.ParseConfig(pgConf.DSN)
	cfg.MinConns = 0
	cfg.MaxConns = 2
	if pgConf.QueryTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = pgConf.QueryTimeout
	} else {
		cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgxpool 构造失败: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping 失败: %w", err)
	}

	return &PGResolver{pool: pool}, nil
}

// Close 释放连接池
func (r *PGResolver) Close() {
	if r != nil && r.pool != nil {
		r.pool.Close()
	}
}

// ResolveTools 按名字列表批量查工具定义。
//
// 缺失的名字静默跳过（不返回错误）——让上游继续对话，只是少了几个工具。
// 这样 agent_service 绑定了一个不存在的工具时不会阻断整条链路。
func (r *PGResolver) ResolveTools(ctx context.Context, names []string) ([]provider.Tool, error) {
	if len(names) == 0 {
		return nil, nil
	}

	rows, err := r.pool.Query(ctx, `
		SELECT name, COALESCE(description,''), COALESCE(params_schema::text,'')
		FROM tools WHERE name = ANY($1) AND status = 1`, names)
	if err != nil {
		return nil, fmt.Errorf("查询工具定义失败: %w", err)
	}
	defer rows.Close()

	var tools []provider.Tool
	for rows.Next() {
		var t provider.Tool
		if err := rows.Scan(&t.Name, &t.Description, &t.Parameters); err != nil {
			return nil, fmt.Errorf("扫描工具定义失败: %w", err)
		}
		tools = append(tools, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历工具结果失败: %w", err)
	}
	return tools, nil
}
