package nas

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSanitizeFileName_路径穿越 文件名来自对方发送方，是**不可信输入**。
func TestSanitizeFileName_路径穿越(t *testing.T) {
	cases := map[string]string{
		// 先取 basename（Node 版也是 basename(name) 起手），再去掉残留的分隔符
		"../../etc/passwd":         "passwd",
		"..\\..\\windows\\sys.ini": "windowssys.ini",
		"/etc/passwd":              "passwd",
		"a/b/c.pdf":                "c.pdf",
		"正常名字.pdf":                 "正常名字.pdf",
		"":                         "file",
		"...":                      "file",
		"  ":                       "file",
	}

	for input, want := range cases {
		if got := SanitizeFileName(input); got != want {
			t.Errorf("SanitizeFileName(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// TestSanitizeFileName_冒号必须去掉 解析器的挂载参数用冒号分隔
// （`docker run -v <路径>:<容器内路径>:ro`）。名字里多一个冒号，挂载目标就整个错位，
// 而症状是「容器里找不到文件」——完全联想不到是文件名的问题。
func TestSanitizeFileName_冒号必须去掉(t *testing.T) {
	if got := SanitizeFileName("报告:2026.pdf"); strings.Contains(got, ":") {
		t.Errorf("冒号没去掉：%q", got)
	}
}

func TestSanitizeFileName_控制字符(t *testing.T) {
	got := SanitizeFileName("a\x00b\x1fc\x7fd.pdf")
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Errorf("控制字符没去掉：%q", got)
		}
	}
}

// TestTruncateFileName_按字节截且留扩展名 名字太长时写文件抛 ENAMETOOLONG，
// 而用户看到的是「文件保存失败」——完全看不出是名字太长。
func TestTruncateFileName_按字节截且留扩展名(t *testing.T) {
	// 中文一字 3 字节：按字符数算会严重低估
	long := strings.Repeat("字", 100) + ".pdf"
	got := TruncateFileName(long, 50)
	if len([]byte(got)) > 50 {
		t.Errorf("截完还有 %d 字节，上限 50：%q", len([]byte(got)), got)
	}
	if !strings.HasSuffix(got, ".pdf") {
		t.Errorf("扩展名被截掉了：%q", got)
	}
}

// TestTruncateFileName_不切半个字 半个 UTF-8 序列是非法字符串。
func TestTruncateFileName_不切半个字(t *testing.T) {
	got := TruncateFileName(strings.Repeat("字", 50), 10)
	for _, r := range got {
		if r == '�' {
			t.Fatalf("切出了半个字符：%q", got)
		}
	}
}

func TestTruncateFileName_短名不动(t *testing.T) {
	if got := TruncateFileName("短.pdf", MaxFileNameBytes); got != "短.pdf" {
		t.Errorf("= %q", got)
	}
}

// TestExtractedPath_保留原扩展名 换成 `.md` 会让 `房产证.pdf` 与 `房产证.docx`
// **撞成同一个落点**——后者会把前者覆盖掉，症状是「两份文件只剩一份的解析结果」。
func TestExtractedPath_保留原扩展名(t *testing.T) {
	pdf, err := ExtractedPath("/root", Location{ID: "abc12345def", OwnerWxid: "wx1", Filename: "房产证.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	docx, err := ExtractedPath("/root", Location{ID: "abc12345def", OwnerWxid: "wx1", Filename: "房产证.docx"})
	if err != nil {
		t.Fatal(err)
	}
	if pdf == docx {
		t.Errorf("两份同名不同扩展名的文件撞到了同一落点：%s", pdf)
	}
	if !strings.HasSuffix(pdf, ".pdf.md") {
		t.Errorf("应保留原扩展名：%s", pdf)
	}
}

// TestExtractedPath_从filepath推导并保持目录结构 手工拷进来的文件可能带着自己的
// 子目录结构（`files/{wxid}/孩子的/KET.pdf`），推导不出来就找不到解析结果。
func TestExtractedPath_从filepath推导并保持目录结构(t *testing.T) {
	got, err := ExtractedPath("/root", Location{
		ID: "abc", OwnerWxid: "wx1", Filename: "KET.pdf",
		Filepath: "files/wx1/孩子的/KET.pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/root", "extracted", "wx1", "孩子的", "KET.pdf.md")
	if got != want {
		t.Errorf("= %s，期望 %s", got, want)
	}
}

// TestExtractedPath_格式不对时安全回退 filepath 不以 files/ 开头时走旧逻辑，
// 而不是拼出一个诡异的路径。
func TestExtractedPath_格式不对时安全回退(t *testing.T) {
	got, err := ExtractedPath("/root", Location{
		ID: "abc12345", OwnerWxid: "wx1", Filename: "a.pdf", Filepath: "随便/什么.pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "abc12345_a.pdf.md") {
		t.Errorf("应回退到 id 前缀的命名：%s", got)
	}
}

func TestExtractedPath_属主非法就报错(t *testing.T) {
	if _, err := ExtractedPath("/root", Location{ID: "abc", OwnerWxid: "..", Filename: "a.pdf"}); err == nil {
		t.Error("路径穿越的属主应当被拒")
	}
}

func TestToRelativePath_不在根下就报错(t *testing.T) {
	if _, err := ToRelativePath("/root", "/elsewhere/a.pdf"); err == nil {
		t.Error("根外的路径不该被接受——那种值在任何机器上都指不准")
	}
	got, err := ToRelativePath("/root", "/root/files/wx/a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("files", "wx", "a.pdf") {
		t.Errorf("= %q", got)
	}
}

func TestAssertValidID_拒绝不合规(t *testing.T) {
	for _, bad := range []string{"../evil", "a/b", "", "a b", "a.b"} {
		if _, err := AssertValidID(bad); err == nil {
			t.Errorf("AssertValidID(%q) 应当拒绝", bad)
		}
	}
	if got, err := AssertValidID("abc-123_XYZ"); err != nil || got != "abc-123_XYZ" {
		t.Errorf("= %q, %v", got, err)
	}
}

// TestSaveOriginal_往返 原件落盘 → 路径推得回来 → md5 是去重键。
func TestSaveOriginal_往返(t *testing.T) {
	root := t.TempDir()
	data := []byte("房产证的内容")

	saved, err := SaveOriginal(SaveOriginalOptions{
		Root: root, Data: data, FileName: "房产证.pdf", OwnerWxid: "wx1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.MD5 != MD5Of(data) {
		t.Errorf("md5 不一致")
	}
	if saved.FileName != "房产证.pdf" {
		t.Errorf("FileName = %q", saved.FileName)
	}
	// 前缀是 id 前 8 位
	if !strings.HasPrefix(filepath.Base(saved.Path), saved.ID[:8]+"_") {
		t.Errorf("路径没有 {id前8}_ 前缀：%s", saved.Path)
	}
	if _, err := os.Stat(saved.Path); err != nil {
		t.Errorf("原件没落盘：%v", err)
	}
	// 相对路径是 files/{wxid}/...
	if !strings.HasPrefix(saved.RelativePath, "files") {
		t.Errorf("RelativePath = %q", saved.RelativePath)
	}
}

func TestSaveOriginal_传已有id不新建(t *testing.T) {
	root := t.TempDir()
	existing := "11111111-2222-4333-8444-555555555555"
	saved, err := SaveOriginal(SaveOriginalOptions{
		Root: root, Data: []byte("x"), FileName: "a.pdf", OwnerWxid: "wx1",
		ID: &existing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != existing {
		t.Errorf("传了 id 却生成了新的：%s != %s", saved.ID, existing)
	}
}

func TestSaveOriginal_属主路径穿越被拒(t *testing.T) {
	root := t.TempDir()
	if _, err := SaveOriginal(SaveOriginalOptions{
		Root: root, Data: []byte("x"), FileName: "a.pdf", OwnerWxid: "..",
	}); err == nil {
		t.Error("属主的路径穿越应当被拒")
	}
}

func TestSaveExtracted_带frontMatter(t *testing.T) {
	root := t.TempDir()
	doc := Location{ID: "abc12345", OwnerWxid: "wx1", Filename: "房产证.pdf",
		Filepath: "files/wx1/房产证.pdf"}

	path, err := SaveExtracted(root, doc, "# 房产证\n\n面积 89 平", map[string]any{
		"Title":      "房产证.pdf",
		"Author":     "",
		"CreateDate": "2020:01:02 03:04:05",
		"PageCount":  2,
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head := string(raw[:strings.Index(string(raw), "# 房产证")])
	if !strings.HasPrefix(head, "---\nfilename: ") {
		t.Errorf("front matter 开头不对：%q", head)
	}
	if !strings.Contains(head, "PageCount: 2") {
		t.Errorf("数字元数据没写进去：%q", head)
	}
	// 空值被跳过（Node 版也是这样：空字符串不进 front matter）
	if strings.Contains(head, "Author:") {
		t.Errorf("空值不该写进 front matter：%q", head)
	}

	// 读回来要**去掉** front matter
	got := ReadExtracted(root, doc)
	if strings.HasPrefix(got, "---") {
		t.Errorf("ReadExtracted 没去掉 front matter：%q", got)
	}
	if !strings.Contains(got, "面积 89 平") {
		t.Errorf("正文丢了：%q", got)
	}
}

// TestWriteAnnotationSummary_用标记行不用标题 正文里完全可能出现 `## 用户批注`
// 这样的标题，按标题定位会在重写时误删正文。
func TestWriteAnnotationSummary_用标记行不用标题(t *testing.T) {
	root := t.TempDir()
	doc := Location{ID: "abc12345", OwnerWxid: "wx1", Filename: "a.pdf",
		Filepath: "files/wx1/a.pdf"}

	// 正文里**本来就有**一个同名标题
	body := "# 房产证\n\n## 用户批注\n\n这是正文里本来就有的标题，不能被删。\n"
	if _, err := SaveExtracted(root, doc, body, nil); err != nil {
		t.Fatal(err)
	}

	if err := WriteAnnotationSummary(root, doc, "## 用户批注\n- 这条是我写的\n"); err != nil {
		t.Fatal(err)
	}

	got := ReadExtracted(root, doc)
	if !strings.Contains(got, "这是正文里本来就有的标题，不能被删。") {
		t.Errorf("正文里同名的标题被误删了：\n%s", got)
	}
	if !strings.Contains(got, "这条是我写的") {
		t.Errorf("批注没写进去：\n%s", got)
	}
	if !strings.Contains(got, AnnotationsMarker) {
		t.Errorf("缺标记行：\n%s", got)
	}
}

// TestWriteAnnotationSummary_刷新不重复 写两次只该有一节。
func TestWriteAnnotationSummary_刷新不重复(t *testing.T) {
	root := t.TempDir()
	doc := Location{ID: "abc12345", OwnerWxid: "wx1", Filename: "a.pdf",
		Filepath: "files/wx1/a.pdf"}
	if _, err := SaveExtracted(root, doc, "正文\n", nil); err != nil {
		t.Fatal(err)
	}

	for _, section := range []string{"## 用户批注\n- 第一条\n", "## 用户批注\n- 第二条\n"} {
		if err := WriteAnnotationSummary(root, doc, section); err != nil {
			t.Fatal(err)
		}
	}

	got := ReadExtracted(root, doc)
	if count := strings.Count(got, AnnotationsMarker); count != 1 {
		t.Errorf("标记行出现了 %d 次，期望 1：\n%s", count, got)
	}
	if strings.Contains(got, "第一条") {
		t.Errorf("旧批注没被替换掉：\n%s", got)
	}
	if !strings.Contains(got, "第二条") {
		t.Errorf("新批注没写进去：\n%s", got)
	}
	if !strings.Contains(got, "正文") {
		t.Errorf("正文丢了：\n%s", got)
	}
}

// TestWriteAnnotationSummary_解析结果不存在也建一份 那时批注是这份文件在盘上
// 唯一的文字线索。
func TestWriteAnnotationSummary_解析结果不存在也建一份(t *testing.T) {
	root := t.TempDir()
	doc := Location{ID: "abc12345", OwnerWxid: "wx1", Filename: "图片.png",
		Filepath: "files/wx1/图片.png"}

	if err := WriteAnnotationSummary(root, doc, "这是一张房产证的照片\n"); err != nil {
		t.Fatalf("解析结果不存在时也该建一份：%v", err)
	}
	got := ReadExtracted(root, doc)
	if !strings.Contains(got, "这是一张房产证的照片") {
		t.Errorf("= %q", got)
	}
}

// TestReadExtracted_保留批注小节 检索与索引正是要靠它搜到批注。
func TestReadExtracted_保留批注小节(t *testing.T) {
	root := t.TempDir()
	doc := Location{ID: "abc12345", OwnerWxid: "wx1", Filename: "a.pdf",
		Filepath: "files/wx1/a.pdf"}
	if _, err := SaveExtracted(root, doc, "正文\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteAnnotationSummary(root, doc, "## 用户批注\n- 关键信息\n"); err != nil {
		t.Fatal(err)
	}

	got := ReadExtracted(root, doc)
	if !strings.Contains(got, "关键信息") {
		t.Errorf("批注小节被剥掉了：\n%s", got)
	}
}

func TestReadExtracted_不存在返回空串(t *testing.T) {
	root := t.TempDir()
	// 「还没解析」是正常状态，不是错误
	if got := ReadExtracted(root, Location{ID: "abc", OwnerWxid: "wx1", Filename: "a.pdf",
		Filepath: "files/wx1/a.pdf"}); got != "" {
		t.Errorf("= %q", got)
	}
}

func TestSplitFrontMatter(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantHead bool
		wantBody string
	}{
		{"正常", "---\nid: x\n---\n\n正文\n", true, "正文\n"},
		{"没有 front matter", "正文\n", false, "正文\n"},
		{"没有结束标记", "---\nid: x\n正文\n", false, "---\nid: x\n正文\n"},
		{"空 front matter", "---\n---\n正文\n", true, "正文\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, body := splitFrontMatter(tc.in)
			if tc.wantHead && head == "" {
				t.Errorf("该有 head：%q", tc.in)
			}
			if !tc.wantHead && head != "" {
				t.Errorf("不该有 head：%q", head)
			}
			if body != tc.wantBody {
				t.Errorf("body = %q，期望 %q", body, tc.wantBody)
			}
		})
	}
}

func TestExtractedBytes_不存在返回0(t *testing.T) {
	root := t.TempDir()
	doc := Location{ID: "abc", OwnerWxid: "wx1", Filename: "a.pdf", Filepath: "files/wx1/a.pdf"}
	if got := ExtractedBytes(root, doc); got != 0 {
		t.Errorf("= %d", got)
	}
}

func TestNewID_UUID形状(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AssertValidID(id); err != nil {
		t.Errorf("生成的 id 竟然不合规：%v", err)
	}
	// 不该重复
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, _ := NewID()
		if seen[id] {
			t.Fatalf("id 撞了：%s", id)
		}
		seen[id] = true
	}
}
