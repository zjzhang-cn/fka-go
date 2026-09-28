// Package ids 校验外部输入的标识符。
//
// ## 为什么单独一个包
//
// 微信 ID 要被用在**两个毫不相干的地方**：拼进向量库那一句没有参数化的查询谓词，
// 以及当落盘目录名（见 internal/nas）。两边各写一套正则的后果已经发生过一次：模型名与
// wxid 曾共用一个字符集，于是 `Xenova/bge-small-zh-v1.5` 因为含斜杠被当成非法值，
// **单元测试全绿、真机第一次跑就炸**。同一个字符集只该有一处定义。
package ids

import (
	"encoding/json"
	"errors"
	"regexp"
)

// wxidPattern 微信 ID 的合法字符集。**刻意不含路径分隔符**。
//
// 字符集里有 `@` 和 `.`，因为真实值形如 `o9cq80_wW26MUpS_bTllNSzseA5k@im.wechat`
// ——只按 `[A-Za-z0-9_-]` 收会把**每一个真实账号都拒掉**。但放宽到 `.` 之后，
// `.` 与 `..` 就能通过字符集了，而它们是**路径穿越**：
// filepath.Join(root, "files", "..") 直接跑到存储根外面去。所以这两个值要单独挡。
var wxidPattern = regexp.MustCompile(`^[A-Za-z0-9_@.-]{1,128}$`)

// IsSafeWxid 判断这个值能否安全地当目录名或拼进谓词。
//
// 除了字符集，还有三个特例要单独挡——它们**都能通过字符集**：
//
//	| 值        | 后果                                              |
//	|-----------|---------------------------------------------------|
//	| . / ..    | 路径穿越：Join(root,"files","..") 跑到存储根外面      |
//	| 前导点     | 在 NAS 上变成隐藏目录，用户以为文件丢了                |
//	| 结尾点     | SMB / Windows 会静默吞掉结尾的点，名字与预期不符        |
func IsSafeWxid(value string) bool {
	if value == "." || value == ".." {
		return false
	}
	if len(value) > 0 && value[0] == '.' {
		return false
	}
	if len(value) > 0 && value[len(value)-1] == '.' {
		return false
	}
	return wxidPattern.MatchString(value)
}

// AssertSafeWxid 校验并返回。
//
// **不合规就返错，不清洗。** 与 nas.assertValidId 同一条思路：清洗会让 `../evil` 变成
// `evil`，与真正的 `evil` 指向同一个目录，两个不同的东西悄然别名到一处。这种错误极难追查。
func AssertSafeWxid(value string, what string) (string, error) {
	if !IsSafeWxid(value) {
		shown := value
		if len(shown) > 60 {
			shown = shown[:60]
		}
		return "", errors.New(what + " 不能用作目录名或查询条件：" + QuoteJSON(shown) +
			"。 合法字符只有字母、数字、下划线、@ . -，且不能是 . 或 ..")
	}
	return value, nil
}

// QuoteJSON 把字符串渲染成 JSON 字符串字面量（含引号）。
//
// 错误信息里的插值与 front matter 都靠它。**手写转义就要枚举转义规则**，
// 而 encoding/json 在这一点上是对的，所以借用它而不是自己拼。
func QuoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// encoding/json 对 string 不会失败；真到了这一步也不能让它 panic
		return `""`
	}
	return string(b)
}
