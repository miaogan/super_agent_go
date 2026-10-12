// Package pagination 统一分页处理，避免各服务各写一套。
package pagination

import (
	"strconv"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
)

const (
	defaultPageSize = 20
	maxPageSize     = 200
)

// Normalize 规整分页参数，防止恶意超大页拖垮数据库
func Normalize(p *commonv1.PageReq) (page, size int) {
	page, size = 1, defaultPageSize
	if p == nil {
		return
	}
	if p.Page > 0 {
		page = int(p.Page)
	}
	if p.PageSize > 0 {
		size = int(p.PageSize)
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	return
}

// Offset 计算 SQL OFFSET
func Offset(p *commonv1.PageReq) int {
	page, size := Normalize(p)
	return (page - 1) * size
}

// Meta 构造分页响应元信息
func Meta(p *commonv1.PageReq, total int) *commonv1.PageMeta {
	page, size := Normalize(p)
	return &commonv1.PageMeta{
		Total:    int32(total),
		Page:     int32(page),
		PageSize: int32(size),
		HasMore:  page*size < total,
	}
}

// Slice 对内存切片做分页，返回本页数据与过滤后的总数。
// mock 仓储与生产 PG 仓储共用同一套分页语义。
func Slice[T any](items []T, p *commonv1.PageReq) (page []T, total int) {
	pageIdx, size := Normalize(p)
	total = len(items)
	start := (pageIdx - 1) * size
	if start >= total {
		return []T{}, total
	}
	end := start + size
	if end > total {
		end = total
	}
	return items[start:end], total
}

// ParseInt 解析字符串为 int，失败返回默认值
func ParseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
