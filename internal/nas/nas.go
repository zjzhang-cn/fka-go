// Package nas 是原始文件与解析结果的落盘。
//
// ## 布局
//
//	{root}/files/{wxid}/{id前8}_{原文件名}
//	{root}/extracted/{wxid}/{rel}.md
//
// 例：
//
//	data/nas/files/o9cq80_.../a676e0be_G3-24 课前测.docx
//	data/nas/extracted/o9cq80_.../a676e0be_G3-24 课前测.docx.md
//
// **与 Node 版逐字一致**——Go 版要能读同一份 data/nas。
//
// ## 为什么按上传者分目录
//
// **为了让 NAS 上能直接读**。原先 `files/a676e0be-.../v1.docx` 完全看不出这是谁的、
// 是什么文件，去 NAS 上翻毫无意义。身份仍由 id 决定，分目录整理的是**存放位置**。
//
// ## 为什么文件名要带 `{id前8}_` 前缀
//
// 同一个人可能发两份**不同内容却同名**的文件（两份「成绩单.pdf」）。不带前缀的话
// 后者会覆盖前者，而数据库里两条记录指向同一个路径——**第一条就变成错的内容**。
// 去重是按 (ownerWxid, contentHash) 做的，挡不住同名不同内容。
//
// 前缀还顺带保住了**路径可推导**：叶子名完全由 ownerWxid + id + filename 决定。
//
// ## 根目录：NAS 优先，缺失时显式回退
//
//	| 情况                    | 根目录                              |
//	|-------------------------|-------------------------------------|
//	| 设了 FILE_STORE_PATH     | 用它（显式指定，压倒一切）            |
//	| NAS_MOUNT_PATH 存在      | {NAS_MOUNT_PATH}/family-knowledge   |
//	| NAS_MOUNT_PATH 不存在    | 回退到 <安装根>/data/nas，**返回 fallback 标记** |
//
// **回退必须是显式的**：调用方拿到 fallback 就该告警。静默回退会让「生产环境忘了挂
// NAS」表现为「文件存在但找不到」——排查时完全不知道从哪下手。
package nas

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/ids"
)

// NASSubdir 挂载点下的子目录名。
const NASSubdir = "family-knowledge"

// RootSource 存储根的来源。
type RootSource string

const (
	// SourceExplicit 环境变量显式指定
	SourceExplicit RootSource = "explicit"
	// SourceNAS NAS 挂载点可用
	SourceNAS RootSource = "nas"
	// SourceFallback 挂载点缺失，落到安装根下
	SourceFallback RootSource = "fallback"
)

// Root 存储根的解析结果。
type Root struct {
	Path string
	// Source 供日志与 CLI 展示
	Source RootSource
	// Detail 回退时的说明
	Detail string
}

// ResolveRoot 解析存储根。**纯函数**（只读环境变量与文件系统状态），便于测试。
func ResolveRoot() Root {
	if explicit := strings.TrimSpace(os.Getenv("FILE_STORE_PATH")); explicit != "" {
		return Root{Path: explicit, Source: SourceExplicit}
	}

	nasMount := strings.TrimSpace(os.Getenv("NAS_MOUNT_PATH"))
	if nasMount == "" {
		nasMount = "/mnt/nas"
	}
	if info, err := os.Stat(nasMount); err == nil && info.IsDir() {
		return Root{Path: filepath.Join(nasMount, NASSubdir), Source: SourceNAS}
	}

	fallback := config.DataPath("nas")
	return Root{
		Path:   fallback,
		Source: SourceFallback,
		Detail: fmt.Sprintf("NAS 挂载点不存在（%s），原始文件将落到 %s。"+
			" 这不是生产配置——部署前请确认 NAS 已挂载，或用 FILE_STORE_PATH 显式指定",
			nasMount, fallback),
	}
}

var (
	pathSeparators = regexp.MustCompile(`[/\\]`)
	controlChars   = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	dotDot         = regexp.MustCompile(`\.\.`)
	docIDPattern   = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

// MaxFileNameBytes 文件名的**字节**上限。
//
// Linux 单个文件名上限是 255 字节，而中文一个字 3 字节、emoji 4 字节——**按字符数
// 算会严重低估**（真实语料里就有 `🌟阅读答题秘籍🌟调整.pdf`）。留出前缀（9 字节）
// 与余量，取 200。
const MaxFileNameBytes = 200

// SanitizeFileName 去掉文件名里会造成路径穿越的部分。
//
// 文件名来自对方发送方，**是不可信输入**。`../../etc/passwd` 这类名字若不处理，
// 一次上传就能写到存储根之外。
//
// ## 冒号也要去掉
//
// Linux 允许文件名里带 `:`，但**解析器的挂载参数用冒号分隔**：
// `docker run -v <路径>:<容器内路径>:ro`。名字里多一个冒号，挂载目标就整个错位，
// 而症状是「容器里找不到文件」——完全联想不到是文件名的问题。
//
// ## 控制字符也去掉
//
// 换行会进日志、NUL 会让写文件直接抛错，两者都不该出现在文件名里。
func SanitizeFileName(name string) string {
	base := pathSeparators.ReplaceAllString(filepath.Base(name), "")
	base = strings.ReplaceAll(base, ":", "")
	base = controlChars.ReplaceAllString(base, "")

	cleaned := dotDot.ReplaceAllString(base, "")
	cleaned = strings.TrimLeft(cleaned, ".")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return "file"
	}
	return cleaned
}

// TruncateFileName 把文件名截到字节上限内，**保留扩展名**。
//
// 不做这件事的后果非常不直观：写文件抛 ENAMETOOLONG，而用户看到的是一句
// 「文件保存失败」——完全看不出是名字太长。
func TruncateFileName(name string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = MaxFileNameBytes
	}
	if len([]byte(name)) <= maxBytes {
		return name
	}

	ext := filepath.Ext(name)
	budget := maxBytes - len([]byte(ext))
	if budget < 1 {
		budget = 1
	}

	// 按码点截，不要把多字节字符切成两半
	var out strings.Builder
	used := 0
	stem := name
	if len([]byte(ext)) < len([]byte(name)) {
		stem = name[:len(name)-len(ext)]
	}
	for _, r := range stem {
		size := len(string(r))
		if used+size > budget {
			break
		}
		out.WriteRune(r)
		used += size
	}

	return out.String() + ext
}

// AssertValidID 校验文档 id。
//
// **拒绝，不清洗。** 与文件名的处理刻意不同：文件名来自发送方，尽量清洗后接受；
// 而 id 由我们自己生成，**不合规就说明是 bug 或注入尝试**。
//
// 清洗在这里反而危险——`../evil` 会被清洗成 `evil`，与真正的 `evil` 指向同一个
// 目录，两个不同的文档悄然别名到一条路径上。这种错误极难追查。
func AssertValidID(id string) (string, error) {
	if !docIDPattern.MatchString(id) {
		return "", fmt.Errorf("非法的文档 id：%s（只允许字母、数字、下划线、连字符）", ids.QuoteJSON(id))
	}
	return id, nil
}

// Location 定位一份文档在存储里的东西。**三个字段缺一不可**——它们共同决定路径。
//
// 收成一个结构而不是三个位置参数：这三个值总是同一份文档的三个字段，散着传迟早
// 会传错顺序，而传错的症状是「路径拼得出来但文件不在」。
type Location struct {
	ID        string
	OwnerWxid string
	Filename  string
	// Filepath 相对存储根的路径。用于从原始文件路径推导提取后路径，保持目录结构一致
	Filepath string
}

// leafName 叶子名：`{id前8}_{文件名}`。
//
// **写入与推导必须走同一个函数**——两处各拼一次的话，改了一处就会出现
// 「写得进去、读不出来」，而那种错很难往「两个地方拼得不一样」上想。
func leafName(id string, filename string) (string, error) {
	valid, err := AssertValidID(id)
	if err != nil {
		return "", err
	}
	prefix := valid
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return prefix + "_" + filename, nil
}

// ToRelativePath 绝对路径 → 相对存储根的路径。**入库时存这个**。
//
// 存相对路径而不是绝对：绝对路径换台机器（存储根不同、NAS 没挂而走了回退）就
// 指向不存在的地方，会让 `/get` 误报「文件不在」。
func ToRelativePath(root string, absolute string) (string, error) {
	rel, err := filepath.Rel(root, absolute)
	if err != nil {
		return "", fmt.Errorf("算相对路径失败（%s 对 %s）：%w", absolute, root, err)
	}
	// 文件总该在根下。真不在就闹出来，别把一个 ../ 存进数据库——
	// 那种值在任何一台机器上都指不准
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("文件不在存储根下：%s（根：%s）", absolute, root)
	}
	return rel, nil
}

// ResolveFilePath 数据库里那列 → 绝对路径。相对值按当前存储根解析，
// 绝对值原样返回（兼容旧数据）。
func ResolveFilePath(root string, filepath_ string) string {
	if filepath_ == "" {
		return root
	}
	if strings.HasPrefix(filepath_, "/") {
		return filepath_
	}
	return filepath.Join(root, filepath_)
}

// OriginalPath 原始文件的路径。
//
// **读数据库那一列（相对路径），不再从 id/属主/文件名推导。**
//
// 推导只在「所有文件都按规范名落在属主目录下」时成立——而手工拷进来的文件可能带着
// 自己的子目录结构（`files/{wxid}/孩子的/KET.pdf`），推导不出来。相对路径同样不怕
// 换机器，所以放弃推导没有代价。
func OriginalPath(root string, doc struct{ Filepath string }) string {
	return ResolveFilePath(root, doc.Filepath)
}

// ExtractedPath 解析结果的路径：**原文件名 + 原扩展名 + `.md`**，目录结构与 `files/`
// 一致。
//
// 例：`files/alice/孩子的/KET.pdf` → `extracted/alice/孩子的/KET.pdf.md`。
//
// ## 为什么保留原扩展名，而不是换成 `.md`
//
// 换成 `.md` 会让 `房产证.pdf` 与 `房产证.docx` **撞成同一个落点**——后者会把前者
// 覆盖掉，症状是「两份文件只剩一份的解析结果」。把原扩展名留在名字里，既避开碰撞，
// 也能一眼看出这份解析结果对应哪个原始文件。
func ExtractedPath(root string, doc Location) (string, error) {
	// 有 filepath 时，从原始文件路径推导，保持目录结构一致。
	// 只接受以 `files/` 开头的相对路径；格式不对时安全地回退到旧逻辑
	if doc.Filepath != "" {
		normalized := strings.ReplaceAll(doc.Filepath, "\\", "/")
		if strings.HasPrefix(normalized, "files/") {
			return filepath.Join(root, "extracted", normalized[len("files/"):]+".md"), nil
		}
	}

	owner, err := ids.AssertSafeWxid(doc.OwnerWxid, "上传者 ID")
	if err != nil {
		return "", err
	}
	leaf, err := leafName(doc.ID, doc.Filename)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "extracted", owner, leaf+".md"), nil
}

// SavedOriginal 落盘结果。
type SavedOriginal struct {
	// ID 文档 id。新建时由本函数生成
	ID string
	// Path 落盘的绝对路径（调用方立刻要用它去解析）
	Path string
	// RelativePath 相对存储根的路径。**入库存这个**
	RelativePath string
	Size         int64
	// MD5 内容 MD5。同时用作去重键
	MD5 string
	// FileName 实际用的文件名（清洗并截断之后）。**入库的 Filename 必须是它**，
	// 否则路径推不回来
	FileName string
}

// SaveOriginalOptions 落盘参数。
type SaveOriginalOptions struct {
	Root     string
	Data     []byte
	FileName string
	// OwnerWxid 上传者。**决定落在哪个目录下**
	OwnerWxid string
	// ID 已有文档的 id。**不传（nil）则新建一个文档**。
	//
	// 用 *string 而不是 string + 空串判断：传空串的调用方本意是「用已有文档」，
	// 走 falsy 分支会**静默生成一个新文档**——数据不会丢，但会悄悄多出一条记录，
	// 而且没人会发现。
	ID *string
}

// SaveOriginal 落盘一个原始文件。
//
// 不做去重判断——**是否重复是业务层的判断**（它要看数据库里的 content_hash），
// 这里只管「把这个字节写到它该在的位置」。
func SaveOriginal(options SaveOriginalOptions) (SavedOriginal, error) {
	id := ""
	if options.ID != nil {
		valid, err := AssertValidID(*options.ID)
		if err != nil {
			return SavedOriginal{}, err
		}
		id = valid
	} else {
		generated, err := NewID()
		if err != nil {
			return SavedOriginal{}, err
		}
		id = generated
	}

	// 顺序要紧：先清洗（去路径成分与冒号）、再截断（按字节）。反过来的话，
	// 截断会按未清洗的字节数算，清洗掉冒号之后又短了
	safeName := TruncateFileName(SanitizeFileName(options.FileName), MaxFileNameBytes)

	owner, err := ids.AssertSafeWxid(options.OwnerWxid, "上传者 ID")
	if err != nil {
		return SavedOriginal{}, err
	}
	dir := filepath.Join(options.Root, "files", owner)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return SavedOriginal{}, fmt.Errorf("建目录失败（%s）：%w", dir, err)
	}

	leaf, err := leafName(id, safeName)
	if err != nil {
		return SavedOriginal{}, err
	}
	path := filepath.Join(dir, leaf)
	if err := os.WriteFile(path, options.Data, 0o644); err != nil {
		return SavedOriginal{}, fmt.Errorf("写原始文件失败（%s）：%w", path, err)
	}

	relative, err := ToRelativePath(options.Root, path)
	if err != nil {
		return SavedOriginal{}, err
	}

	digest := md5.Sum(options.Data)
	md5hex := hex.EncodeToString(digest[:])

	config.Log().Info("原始文件已落盘", config.Context{
		"id": id, "ownerWxid": options.OwnerWxid, "path": path, "bytes": len(options.Data),
	})

	return SavedOriginal{
		ID: id, Path: path, RelativePath: relative,
		Size: int64(len(options.Data)), MD5: md5hex, FileName: safeName,
	}, nil
}

// NewID 生成一个文档 id。**UUID 形状**（8-4-4-4-12）。
func NewID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成文档 id 失败：%w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// MD5Of 算内容 MD5。**去重键**。
func MD5Of(data []byte) string {
	digest := md5.Sum(data)
	return hex.EncodeToString(digest[:])
}

// SaveExtracted 落盘解析结果。
//
// 与原始文件分开目录：**重新解析时不该动原始文件**。改了 MarkItDown 版本、或换了
// 解析参数，只需重跑 extracted/，原始文件一直是 Source of Truth。
//
// 写入时在文件开头附加 YAML front matter，把元数据保存在文档自身中。
func SaveExtracted(root string, doc Location, markdown string, metadata map[string]any) (string, error) {
	path, err := ExtractedPath(root, doc)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("建目录失败（%s）：%w", filepath.Dir(path), err)
	}

	body := buildFrontMatter(doc, metadata) + markdown
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("写解析结果失败（%s）：%w", path, err)
	}

	config.Log().Debug("解析结果已落盘", config.Context{
		"id": doc.ID, "path": path, "bytes": len([]rune(markdown)),
	})
	return path, nil
}

// buildFrontMatter YAML front matter 文本（含结尾换行）。SaveExtracted 与批注重写共用。
//
// 值一律走 JSON 编码（`JSON.stringify` 的形状）：**手写 YAML 转义就要枚举规则**，
// 而引号内的中文与标点本来就安全——front matter 是给自己读回��，不是给人手编的。
func buildFrontMatter(doc Location, metadata map[string]any) string {
	lines := []string{
		"---",
		"filename: " + ids.QuoteJSON(doc.Filename),
		"ownerWxid: " + ids.QuoteJSON(doc.OwnerWxid),
		"id: " + ids.QuoteJSON(doc.ID),
	}

	// 追加 ExifTool 提取的元数据。**顺序按 key 排**——Map 遍历是随机的，
	// 而随机顺序会让同一份文档每次写出的字节都不同，diff 与缓存全废
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sortStrings(keys)
	for _, key := range keys {
		value := metadata[key]
		if value == nil || value == "" {
			continue
		}
		lines = append(lines, key+": "+JSONValue(value))
	}

	lines = append(lines, "---", "")
	return strings.Join(lines, "\n")
}

// AnnotationsMarker 批注小节的标记行。
//
// **用 HTML 注释而不是标题**：正文里完全可能出现 `## 用户批注` 这样的标题，按标题
// 定位会在重写时误删正文。标记行由我们生成，不会与解析结果撞上。
const AnnotationsMarker = "<!-- fka:annotations -->"

// WriteAnnotationSummary 把「用户批注」小节写进 extracted（或刷新已存在的小节），
// **其余内容原样保留**。
//
// 批注是用户输入、存在数据库里；这里只是把它**派生**进 NAS 上的解析结果，让关键词
// 检索与向量索引能顺带搜到（它们都从 extracted 读）。所以重跑解析覆盖掉这一节也没
// 关系——下一次写批注会重建它。传空串 = 移除已有小节。
//
// 解析结果还不存在（图片、扫描件提不出正文，或还没解析）时**照样建一份**：那时批注
// 是这份文件在盘上唯一的文字线索。
func WriteAnnotationSummary(root string, doc Location, section string) error {
	path, err := ExtractedPath(root, doc)
	if err != nil {
		return err
	}
	block := renderAnnotationsBlock(section)

	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("读解析结果失败（%s）：%w", path, err)
		}
		// 还不存在：建一份
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("建目录失败（%s）：%w", filepath.Dir(path), err)
		}
		return os.WriteFile(path, []byte(buildFrontMatter(doc, nil)+block), 0o644)
	}

	head, body := splitFrontMatter(string(raw))
	without := stripAnnotationSection(body)

	// 正文与批注小节之间空一行；正文本就以换行结尾时补一个即可
	separator := ""
	if without != "" && block != "" {
		if strings.HasSuffix(without, "\n") {
			separator = "\n"
		} else {
			separator = "\n\n"
		}
	}

	if err := os.WriteFile(path, []byte(head+without+separator+block), 0o644); err != nil {
		return fmt.Errorf("写批注小节失败（%s）：%w", path, err)
	}

	config.Log().Debug("用户批注已写入解析结果", config.Context{
		"id": doc.ID, "path": path, "bytes": len([]rune(block)),
	})
	return nil
}

// renderAnnotationsBlock 批注小节文本。空串 = 不写任何东西（用于移除）。
func renderAnnotationsBlock(section string) string {
	trimmed := strings.TrimSpace(section)
	if trimmed == "" {
		return ""
	}
	return AnnotationsMarker + "\n" + trimmed + "\n"
}

// stripAnnotationSection 去掉正文里已有的批注小节（从标记行到文件末尾），并去掉尾部空白。
func stripAnnotationSection(body string) string {
	at := strings.Index(body, AnnotationsMarker)
	if at < 0 {
		return body
	}
	return strings.TrimRight(body[:at], " \t\n\r")
}

// ReadExtracted 读回解析结果。**不存在返回空串**——「还没解析」是正常状态，不是错误。
//
// 返回的内容**已去掉 YAML front matter**——调用方需要的是正文，不是元数据。用户
// 批注小节**保留在正文里**：检索与索引正是要靠它搜到批注。
func ReadExtracted(root string, doc Location) string {
	path, err := ExtractedPath(root, doc)
	if err != nil {
		config.Log().Warn("解析结果路径算不出来", config.Context{"id": doc.ID, "error": err.Error()})
		return ""
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			config.Log().Warn("解析结果读取失败", config.Context{"id": doc.ID, "error": err.Error()})
		}
		return ""
	}
	_, body := splitFrontMatter(string(raw))
	return body
}

// splitFrontMatter 拆出 YAML front matter 与正文。
//
// Head 含 front matter 到正文前那一段（以换行结尾），Body 是正文。ReadExtracted 与
// 批注重写共用它——两处各判一次的话，改了一处就会出现「写得进去、读不出来」。
func splitFrontMatter(content string) (head string, body string) {
	if !strings.HasPrefix(content, "---") {
		return "", content
	}

	lines := strings.Split(content, "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return "", content
	}

	// 跳过结束标记后的空行，正文从第一个非空行开始
	start := end + 1
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}

	head = strings.Join(lines[:start], "\n") + "\n"
	return head, strings.Join(lines[start:], "\n")
}

// ExtractedBytes 一份文档解析结果的字节数。文件不在（还没解析、或 extracted 被删）
// 时返回 0——「还没解析」是正常状态，不是错误。
func ExtractedBytes(root string, doc Location) int64 {
	path, err := ExtractedPath(root, doc)
	if err != nil {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// ExtractedDirBytes extracted/ 下所有解析结果的总字节数。db stats 的存储统计用。
func ExtractedDirBytes(root string) int64 {
	return dirBytes(filepath.Join(root, "extracted"))
}

func dirBytes(dir string) int64 {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			total += dirBytes(path)
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		// 读不到就跳过这一份，总量略小好过整个命令失败
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// OriginalSize 已落盘的原始文件大小。用于对账（数据库记录 vs 实际文件）。
func OriginalSize(path string) *int64 {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	size := info.Size()
	return &size
}

// JSONValue 把任意标量渲染成 front matter 里的一行值。
//
// 走 encoding/json 而不是手拼：**手写就要枚举转义规则**，而元数据来自 ExifTool
// （文件名里什么字符都可能有）。数字与布尔不加引号——那是它们本来的样子；字符串
// 加引号，避免 "false" 之类被读成布尔。
func JSONValue(value any) string {
	switch v := value.(type) {
	case string:
		return ids.QuoteJSON(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprintf("%v", v)
	case nil:
		return "null"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ids.QuoteJSON(fmt.Sprintf("%v", value))
	}
	return string(encoded)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
