// 文档的关键词检索。
//
// ## 为什么直接扫 md 文件
//
// **正文不落库**，数据来源是 NAS 上 `extracted/` 里的 Markdown。先用库筛出
// 「已解析 + 有权限」的候选（元数据），再逐份读它的 `.md` 匹配。
//
// 三个刻意的选择：
//
//  1. **直接扫 md，不用 FTS5 也不存正文副本。** 家庭场景下文档量小，逐份读 md
//     做子串匹配够用；正文只存在 NAS 一处，就没有「库与盘哪个是真的」的问题。
//     等真觉得慢了再谈索引——换的时候只动这个文件。
//  2. **权限过滤交给存储层。** 候选来自 `ListDocuments(ViewerWxid: …)`，过滤在
//     那一句 SQL 的 WHERE 里完成。查完再筛的话，将来有人改查询时漏掉那一句，
//     private 内容就漏出去了——**那不是「显示错」，是「本来不该参与检索的内容
//     参与了」**。
//  3. **读盘取正文，不经过服务。** 正文是 NAS 上的文件，任何进程都能读。
//
// ## 词之间是「且」
//
// 每个词都要出现在同一份文档里（文件名或正文**任一**处）。这与记忆检索同一套
// 规则，见 internal/searchterms。
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/zjzhang-cn/fka-go/mcp/internal/nas"
	"github.com/zjzhang-cn/fka-go/mcp/internal/searchterms"
	"github.com/zjzhang-cn/fka-go/mcp/internal/store"
)

// SnippetRadius 上下文片段两侧各取多少字符。
//
// 40 是个折中：够看清「这句话在讲什么」，又不至于让一条结果占掉半屏。
const SnippetRadius = 40

// DefaultLimit 默认返回多少条。比 `db list` 小——每条要带片段，占的屏幕高得多。
const DefaultLimit = 20

// ListFunc 「列出候选文档」这一个动作。收窄成这个形状，不必为了检索暴露整个
// 存储层。
type ListFunc func(ctx context.Context, in store.ListDocumentsInput) (store.ListDocumentsResult, error)

// MatchedIn 命中在哪。文件名与正文都命中时是 both。
type MatchedIn string

const (
	MatchedFilename MatchedIn = "filename"
	MatchedContent  MatchedIn = "content"
	MatchedBoth     MatchedIn = "both"
)

// SearchOptions 检索参数。
type SearchOptions struct {
	// StorageRoot 解析结果（extracted/*.md）在这里，正文匹配要读它
	StorageRoot string
	// Query 查询词。**空格分隔的多个词之间是「且」**
	Query string
	// ViewerWxid 提问者。**必填**——权限过滤靠它
	ViewerWxid string
	Limit      int
}

// Hit 一条命中。
type Hit struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	// Filepath **相对存储根**的路径。`send_document` 靠它定位文件
	Filepath  string `json:"filepath"`
	Size      *int64 `json:"size"`
	OwnerWxid string `json:"ownerWxid"`
	// Visibility 给模型看的，多半是为了让它知道能不能引用
	Visibility string    `json:"visibility"`
	CreatedAt  int64     `json:"createdAt"`
	Matched    MatchedIn `json:"matched"`
	// Snippet 正文里命中处的上下文片段。正文没命中时为 nil
	Snippet *string `json:"snippet"`
}

// SearchResult 检索结果。
type SearchResult struct {
	Hits []Hit `json:"hits"`
	// Total 命中的总数（不受 Limit 影响）
	Total int `json:"total"`
	// Truncated 是否因为 Limit 被截断
	Truncated bool `json:"truncated"`
	// Terms 实际参与检索的词
	Terms []string `json:"terms"`
	// ScopedTo 按谁的身份过滤的。空 = 未过滤
	ScopedTo string `json:"scopedTo"`
}

// ExtractSnippet 取正文里命中处的上下文片段。
//
// **压成一行**，不是原样截取。正文是 Markdown，片段里若带换行，塞进列表之后
// 「一个元素其实是两行」——按行做断言的测试就永远测不到想测的东西。
//
// 多个词命中时取**最靠前**的那个做中心：从前往后读更符合「这文件在讲什么」的直觉。
//
// 纯函数，便于测试。
func ExtractSnippet(content string, terms []string, radius int) *string {
	if content == "" {
		return nil
	}

	flat := strings.Join(strings.Fields(content), " ")
	lower := strings.ToLower(flat)

	at := -1
	hitLength := 0
	for _, term := range terms {
		index := strings.Index(lower, strings.ToLower(term))
		if index >= 0 && (at < 0 || index < at) {
			at = index
			hitLength = len([]rune(term))
		}
	}
	if at < 0 {
		return nil
	}

	start := at - radius
	if start < 0 {
		start = 0
	}
	end := at + hitLength + radius
	if end > len(flat) {
		end = len(flat)
	}

	head, tail := "", ""
	if start > 0 {
		head = "…"
	}
	if end < len(flat) {
		tail = "…"
	}
	out := head + flat[start:end] + tail
	return &out
}

// Search 检索。
//
// 查询词为空时返错而不是返回全库——「搜空词 = 列出所有文档」是个危险的默认值，
// 调用方（CLI / 问答）应当各自拦在前面。
func Search(ctx context.Context, list ListFunc, options SearchOptions) (SearchResult, error) {
	terms := searchterms.SplitTerms(options.Query)
	if len(terms) == 0 {
		return SearchResult{}, fmt.Errorf("检索词不能为空")
	}

	limit := options.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	lowerTerms := make([]string, 0, len(terms))
	for _, term := range terms {
		lowerTerms = append(lowerTerms, strings.ToLower(term))
	}

	// 未解析完的文档不参与检索；权限过滤交给存储层（进 WHERE）
	listed, err := list(ctx, store.ListDocumentsInput{
		Status:     "ready",
		Limit:      1 << 30, // 检索要全量过一遍，截断交给最后的 limit
		ViewerWxid: options.ViewerWxid,
	})
	if err != nil {
		return SearchResult{}, err
	}

	hits := make([]Hit, 0, len(listed.Rows))

	for _, row := range listed.Rows {
		location := nas.Location{
			ID: row.ID, OwnerWxid: row.OwnerWxid,
			Filename: row.Filename, Filepath: row.Filepath,
		}

		// 逐份读 md。读不到按「没有正文」处理——文件名仍可命中
		content := nas.ReadExtracted(options.StorageRoot, location)
		contentLower := strings.ToLower(content)

		nameLower := strings.ToLower(row.Filename)
		nameHit := containsAll(nameLower, lowerTerms)
		contentHit := content != "" && containsAll(contentLower, lowerTerms)

		// 词之间是「且」，每个词各自出现在文件名**或**正文里
		perTerm := make([]bool, 0, len(lowerTerms))
		for _, term := range lowerTerms {
			perTerm = append(perTerm, strings.Contains(nameLower, term) || strings.Contains(contentLower, term))
		}
		if !containsAllInBools(perTerm) {
			continue
		}

		matched := MatchedContent
		switch {
		case nameHit && contentHit:
			matched = MatchedBoth
		case nameHit:
			matched = MatchedFilename
		}

		var snippet *string
		if contentHit {
			snippet = ExtractSnippet(content, terms, SnippetRadius)
		}

		hits = append(hits, Hit{
			ID: row.ID, Filename: row.Filename, Filepath: row.Filepath,
			Size: row.Size, OwnerWxid: row.OwnerWxid, Visibility: row.Visibility,
			CreatedAt: row.CreatedAt, Matched: matched, Snippet: snippet,
		})
	}

	// 排序：文件名全部命中的排前面，其余保持库给的归档时间倒序
	// （稳定排序保持同档内的可解释顺序）
	stableSortByFilenameFirst(hits)

	total := len(hits)
	truncated := total > limit
	if truncated {
		hits = hits[:limit]
	}

	return SearchResult{
		Hits: hits, Total: total, Truncated: truncated,
		Terms: terms, ScopedTo: options.ViewerWxid,
	}, nil
}

// containsAll 每个词都要出现。
func containsAll(haystack string, needles []string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}

func containsAllInBools(values []bool) bool {
	for _, value := range values {
		if !value {
			return false
		}
	}
	return true
}

// stableSortByFilenameFirst 把「仅正文命中」的排到后面。**稳定**——同档内保持
// 库给的归档时间倒序。
func stableSortByFilenameFirst(hits []Hit) {
	// 插入排序：n 很小（家庭规模几十份），而它的稳定性是白送的
	for i := 1; i < len(hits); i++ {
		current := hits[i]
		if current.Matched != MatchedContent {
			continue
		}
		j := i - 1
		for j >= 0 && hits[j].Matched == MatchedContent {
			hits[j+1] = hits[j]
			j--
		}
		hits[j+1] = current
	}
}
