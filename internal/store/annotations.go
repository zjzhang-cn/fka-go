package store

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Annotation 一条用户批注：用户**引用自己上传的文件消息并写字**时，那段话。
//
// 字段刻意只有三个——它是一句话的记账，不是一张内容表。可见性随文档，
// 不在这里单独存。
type Annotation struct {
	// Text 用户写的那段话（已 trim）
	Text string `json:"text"`
	// AuthorWxid 写下它的用户（全局唯一标识）
	AuthorWxid string `json:"authorWxid"`
	// CreatedAt Unix 毫秒
	CreatedAt int64 `json:"createdAt"`
}

// ParseAnnotations 一列 JSON 文本 → 批注数组。**空值、坏值都当空数组，绝不返错。**
//
// 解析刻意**容错**：一列坏 JSON 不该让整份文档读不出来（列表、检索、`db show`
// 都会读这一列）。读不出来的元素直接丢掉，返回能用的那部分——**少看一条批注
// 远好过整份文档打不开**。这与「原始文件是不可信输入要清洗」是同一条思路。
func ParseAnnotations(raw any) []Annotation {
	switch value := raw.(type) {
	case nil:
		return nil
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil
		}
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			return nil
		}
		return coerceAnnotations(decoded)
	case []byte:
		return ParseAnnotations(string(value))
	default:
		// 调用方可能已经解过一遍（例如从 JSON 列直读）
		return coerceAnnotations(value)
	}
}

func coerceAnnotations(value any) []Annotation {
	list, ok := value.([]any)
	if !ok {
		return nil
	}

	out := make([]Annotation, 0, len(list))
	for _, item := range list {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}

		text, ok := object["text"].(string)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		author, ok := object["authorWxid"].(string)
		if !ok || author == "" {
			continue
		}
		createdAt, ok := toInt64(object["createdAt"])
		if !ok {
			continue
		}

		out = append(out, Annotation{Text: text, AuthorWxid: author, CreatedAt: createdAt})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toInt64 宽松地取整数。JSON 里的数字解出来是 float64，而有些实现会给字符串。
func toInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

// SerializeAnnotations 批注数组 → 一列 JSON 文本。**空数组写 NULL**——
// 「没有批注」用 NULL 表示，不写 `[]`。
//
// 字段名沿用 camelCase（`authorWxid` / `createdAt`）而不是 snake_case：这一列是
// Node 版写下的，已有数据在用 camelCase，换名字会让历史批注全部读不出来。
func SerializeAnnotations(annotations []Annotation) any {
	if len(annotations) == 0 {
		return nil
	}
	encoded, err := json.Marshal(annotations)
	if err != nil {
		return nil
	}
	return string(encoded)
}
