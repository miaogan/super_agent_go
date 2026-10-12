package meta

import (
	"sort"
	"strconv"
	"strings"

	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"
)

// FormatChunkID 生成切片 ID（对外导出供 ingest 流水线使用）。
//
// 格式：d<docID>_<seq> —— 自解释且便于按文档前缀排查。
// 真实实现用 UUID 或雪花 ID；这里选可读格式是因为冒烟脚本
// 需要肉眼可判断的切片标识。
func FormatChunkID(docID int64, seq int) string {
	return "d" + strconv.FormatInt(docID, 10) + "_" + strconv.Itoa(seq)
}

func sortKBs(kbs []*ragv1.KnowledgeBase) {
	sort.Slice(kbs, func(i, j int) bool {
		return kbs[i].GetKbId() < kbs[j].GetKbId()
	})
}

func paginateKBs(items []*ragv1.KnowledgeBase, page, size int) ([]*ragv1.KnowledgeBase, int) {
	total := len(items)
	start := (page - 1) * size
	if start >= total {
		return []*ragv1.KnowledgeBase{}, total
	}
	end := start + size
	if end > total {
		end = total
	}
	return items[start:end], total
}

func paginateDocs(items []*ragv1.Document, page, size int) ([]*ragv1.Document, int) {
	total := len(items)
	start := (page - 1) * size
	if start >= total {
		return []*ragv1.Document{}, total
	}
	end := start + size
	if end > total {
		end = total
	}
	return items[start:end], total
}

// ScorePair 分数条目
type ScorePair struct {
	ID    string
	Score float64
}

// SortScorePairs 按分数降序排序；同分按 ID 保证结果稳定可复现
func SortScorePairs(list []ScorePair) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Score != list[j].Score {
			return list[i].Score > list[j].Score
		}
		return list[i].ID < list[j].ID
	})
}

// tokenize 极简分词。
//
// 中文按二元组（bigram）切分，英文数字按空格切分。
// 这是内存降级检索用的启发式实现；PostgreSQL 版本会用 tsvector。
func tokenize(s string) []string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return nil
	}

	var out []string
	runes := []rune(s)

	// ASCII 词（连续字母数字）
	var buf strings.Builder
	flush := func() {
		if buf.Len() >= 2 {
			out = append(out, buf.String())
		}
		buf.Reset()
	}
	for _, r := range runes {
		if isASCIIWord(r) {
			buf.WriteRune(r)
			continue
		}
		flush()
	}
	flush()

	// 中文 bigram
	for i := 0; i+1 < len(runes); i++ {
		if isCJK(runes[i]) && isCJK(runes[i+1]) {
			out = append(out, string(runes[i:i+2]))
		}
	}
	// 单字 CJK 也保留，覆盖单字查询
	for _, r := range runes {
		if isCJK(r) {
			out = append(out, string(r))
		}
	}
	return out
}

func isASCIIWord(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF)
}
