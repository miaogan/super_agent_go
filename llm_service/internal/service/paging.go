package service

import (
	"strings"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
)

// pageSize 返回规整后的页大小
func pageSize(p *commonv1.PageReq) int {
	_, size := normalizePage(p)
	return size
}

// paginate 对切片分页
func paginate[T any](items []T, p *commonv1.PageReq) (page int, total int) {
	pageIdx, size := normalizePage(p)
	total = len(items)
	start := (pageIdx - 1) * size
	if start >= total {
		return pageIdx, total
	}
	end := start + size
	if end > total {
		end = total
	}
	_ = items[start:end]
	return pageIdx, total
}

func normalizePage(p *commonv1.PageReq) (page, size int) {
	page, size = 1, 20
	if p == nil {
		return
	}
	if p.GetPage() > 0 {
		page = int(p.GetPage())
	}
	if p.GetPageSize() > 0 {
		size = int(p.GetPageSize())
	}
	if size > 200 {
		size = 200
	}
	return
}

// trimQuote 去掉首尾空白与配对的引号。
//
// OAuth2.0 / HTTP 头里的令牌常被误带引号（如从 JSON 手工拷出），
// 这里统一清理，避免"看起来对但校验失败"的诡异问题。
func trimQuote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
