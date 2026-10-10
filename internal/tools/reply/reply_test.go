// 本文件钉住 reply 源的两件事：它是**输出通道**（能走到 Reply），
// 以及它是**受限的**（路径必须留在发送根内、只能是普通文件、没会话时好好说话）。
//
// 路径那几条尤其重要：提示注入是这份产品的结构性暴露，而 Reply.File 收的是本机
// 路径——少一道闸就等于给了一个任意文件外带口，且失败时不报错。
package reply

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// fakeReply 记下被要求发了什么，不真的发。
type fakeReply struct {
	texts  []string
	files  []string
	images []string
}

func (f *fakeReply) Text(ctx context.Context, text string) error {
	f.texts = append(f.texts, text)
	return nil
}
func (f *fakeReply) File(ctx context.Context, path, fileName string) error {
	f.files = append(f.files, path)
	return nil
}
func (f *fakeReply) Image(ctx context.Context, path, fileName string) error {
	f.images = append(f.images, path)
	return nil
}

// newRoot 造一个带一个普通文件的发送根，返回根与那个文件的相对路径。
func newRoot(t *testing.T) (root, rel string) {
	t.Helper()
	root = t.TempDir()
	rel = "a.txt"
	if err := os.WriteFile(filepath.Join(root, rel), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, rel
}

func call(t *testing.T, s tools.Source, tc tools.Context, name string, args map[string]any) tools.Result {
	t.Helper()
	res, err := s.Call(context.Background(), name, args, tc)
	if err != nil {
		t.Fatalf("Call(%s) 抛错：%v", name, err)
	}
	return res
}

// Test源_三个工具都是send类别 默认只读策略下它们一个都看不到；要开放必须显式
// 给 LLM_TOOL_EFFECTS 加 send。这条钉住「默认关着」是声明出来的，不是碰巧。
func Test源_三个工具都是send类别(t *testing.T) {
	s := NewSource(Options{Root: t.TempDir()})
	specs, err := s.List(context.Background(), tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 {
		t.Fatalf("该有 3 个工具，实际 %d", len(specs))
	}
	for _, spec := range specs {
		if spec.Effect != tools.EffectSend {
			t.Errorf("%s 的类别是 %s，该是 send", spec.Name, spec.Effect)
		}
	}
}

// Test发文字_走Reply。
func Test发文字_走Reply(t *testing.T) {
	s := NewSource(Options{Root: t.TempDir()})
	r := &fakeReply{}

	res := call(t, s, tools.Context{Reply: r}, ToolText, map[string]any{"text": "先发这个"})
	if !res.OK {
		t.Fatalf("该成功：%s", res.Content)
	}
	if len(r.texts) != 1 || r.texts[0] != "先发这个" {
		t.Errorf("文字没到 Reply：%v", r.texts)
	}
}

// Test发文件_普通文件放行。
func Test发文件_普通文件放行(t *testing.T) {
	root, rel := newRoot(t)
	s := NewSource(Options{Root: root})
	r := &fakeReply{}

	res := call(t, s, tools.Context{Reply: r}, ToolFile, map[string]any{"path": rel})
	if !res.OK {
		t.Fatalf("该成功：%s", res.Content)
	}
	if len(r.files) != 1 || !strings.HasSuffix(r.files[0], rel) {
		t.Errorf("文件没到 Reply：%v", r.files)
	}
}

// Test发图片_走Image 图片与文件走的是 Reply 的不同方法（渠道据此退化成文件）。
func Test发图片_走Image(t *testing.T) {
	root, rel := newRoot(t)
	s := NewSource(Options{Root: root})
	r := &fakeReply{}

	call(t, s, tools.Context{Reply: r}, ToolImage, map[string]any{"path": rel})
	if len(r.images) != 1 {
		t.Errorf("图片没走 Image：%+v", r)
	}
	if len(r.files) != 0 {
		t.Errorf("图片不该走 File：%+v", r)
	}
}

// Test路径越界被拒 绝对路径、..、符号链接逃逸三条路都要挡。
func Test路径越界被拒(t *testing.T) {
	root, _ := newRoot(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 根内放一个指向根外的软链
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	s := NewSource(Options{Root: root})
	r := &fakeReply{}
	for _, bad := range []string{
		outside,         // 绝对路径
		"../secret.txt", // 相对但越界
		"link",          // 软链逃逸
	} {
		res := call(t, s, tools.Context{Reply: r}, ToolFile, map[string]any{"path": bad})
		if res.OK {
			t.Errorf("路径 %q 该被拒，却放行了", bad)
		}
	}
	if len(r.files) != 0 {
		t.Errorf("被拒的路径不该到 Reply：%v", r.files)
	}
}

// Test不是普通文件被拒 目录不能发。
func Test不是普通文件被拒(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewSource(Options{Root: root})
	r := &fakeReply{}

	res := call(t, s, tools.Context{Reply: r}, ToolFile, map[string]any{"path": "sub"})
	if res.OK {
		t.Errorf("目录不该被当作可发文件：%s", res.Content)
	}
}

// Test超过大小上限被拒 上限是「让远端决定我们读多少内存」的反面——这里读文件的是
// 渠道，但路径来自模型，所以大小检查要在这里先做。
func Test超过大小上限被拒(t *testing.T) {
	root := t.TempDir()
	rel := "big.bin"
	if err := os.WriteFile(filepath.Join(root, rel), make([]byte, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSource(Options{Root: root, MaxBytes: 16})
	r := &fakeReply{}

	res := call(t, s, tools.Context{Reply: r}, ToolFile, map[string]any{"path": rel})
	if res.OK {
		t.Errorf("超过上限该被拒：%s", res.Content)
	}
}

// Test没有会话时给一句话 没有 Reply（fka ask 那条路）不是异常，也不能 panic——
// 要给模型一句能照做的话：把内容写进最终答案。
func Test没有会话时给一句话(t *testing.T) {
	s := NewSource(Options{Root: t.TempDir()})

	res, err := s.Call(context.Background(), ToolText, map[string]any{"text": "hi"}, tools.Context{})
	if err != nil {
		t.Fatalf("不该抛错：%v", err)
	}
	if res.OK {
		t.Fatalf("没有会话时不该成功：%s", res.Content)
	}
	if !strings.Contains(res.Content, "最终答案") {
		t.Errorf("该告诉模型改走最终答案，实际：%s", res.Content)
	}
}

// Test文字为空被拒。
func Test文字为空被拒(t *testing.T) {
	s := NewSource(Options{Root: t.TempDir()})
	res := call(t, s, tools.Context{Reply: &fakeReply{}}, ToolText, map[string]any{"text": "  "})
	if res.OK {
		t.Errorf("空文字该被拒：%s", res.Content)
	}
}

// TestPromptSection_给出根 模型得知道相对谁，否则只能瞎猜路径。
func TestPromptSection_给出根(t *testing.T) {
	root := t.TempDir()
	s := NewSource(Options{Root: root})

	section, err := s.PromptSection(tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(section, root) {
		t.Errorf("说明里该给出根路径 %q，实际：%s", root, section)
	}
}
