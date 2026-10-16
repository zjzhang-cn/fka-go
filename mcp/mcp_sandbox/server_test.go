// 本文件是**工具层**的用例：参数怎么读、结果怎么说给人听。
//
// 这一层最容易出的错**不报错**：声明与读取方式不一致时（`mcp.WithNumber` 声明、
// `GetString` 读），参数会静默失效——照样返回结果，只是超时不由调用方定。
package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// testPrincipal 测试用的租户身份。多租户下缺身份会被拒，所以默认请求都带它。
const testPrincipal = "test"

// callRequest 造一个工具调用请求。**数字参数用 float64**：JSON 解出来的数字就是
// 这个类型，用 `"1"` 去测会把「声明是 number 却按 string 读」那个 bug 测没。
func callRequest(args map[string]any) mcp.CallToolRequest {
	return callRequestAs(testPrincipal, args)
}

// callRequestAs 指定身份（空串 = 不带身份，用于验 fail-closed）。
func callRequestAs(principal string, args map[string]any) mcp.CallToolRequest {
	var request mcp.CallToolRequest
	request.Params.Arguments = args
	if principal != "" {
		request.Params.Meta = mcp.NewMetaFromMap(map[string]any{metaPrincipalKey: principal})
	}
	return request
}

// newTestServer 造一个把 testPrincipal **直接映射到 s** 的 server：用例把文件种在
// s.Root 里，所以让 test 租户的根就是 s.Root（不走 `<根>/<user>` 那一层）。
func newTestServer(s *Sandbox) *sandboxServer {
	return &sandboxServer{
		opts:      Options{Root: s.Root},
		sandboxes: map[string]*Sandbox{testPrincipal: s},
	}
}

func withExecLog(s *sandboxServer, execLog *ExecLog) *sandboxServer {
	s.execLog = execLog
	return s
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("结果里没有内容")
	}
	return mcp.GetTextFromContent(result.Content[0])
}

// TestRun工具_缺command要拒 没有命令就没得跑——回一句给模型看的话，不抛异常。
func TestRun工具_缺command要拒(t *testing.T) {
	impl := newTestServer(newTestSandbox(t))

	result, err := impl.handleRun(context.Background(), callRequest(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("缺 command 该回一条 IsError 的结果")
	}
	if body := resultText(t, result); !strings.Contains(body, "command") {
		t.Errorf("该说清缺的是哪个参数：%s", body)
	}
}

// TestRun工具_超时参数按数字读 声明是 `mcp.WithNumber`，读取就必须用 `GetInt`：
// `GetString` 只认 Go string，而 JSON 数字是 float64——于是超时永远取默认值。
// 用 `sleep 5` + `timeout_sec: 1` 钉住：读对了 1 秒就超时，读错了会等满 5 秒。
func TestRun工具_超时参数按数字读(t *testing.T) {
	requireBash(t)
	impl := newTestServer(newTestSandbox(t))

	result, err := impl.handleRun(context.Background(), callRequest(map[string]any{
		"command":     "sleep 5",
		"timeout_sec": float64(1),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("超时该回 IsError 的结果")
	}
	if body := resultText(t, result); !strings.Contains(body, "超时") {
		t.Errorf("该说清是超时：%s", body)
	}
}

// TestRun工具_成功结果不带IsError 命令跑完、退出码 0，是正常结果。
func TestRun工具_成功结果不带IsError(t *testing.T) {
	requireBash(t)
	impl := newTestServer(newTestSandbox(t))

	result, err := impl.handleRun(context.Background(), callRequest(map[string]any{
		"command": `printf sandbox-ok`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Errorf("正常执行不该是 IsError：%s", resultText(t, result))
	}
	if body := resultText(t, result); !strings.Contains(body, "sandbox-ok") {
		t.Errorf("结果里该有命令输出：%s", body)
	}
}

// TestRun工具_策略拒绝是IsError 被策略拦下的命令是「没能跑起来」，该让模型看到原因。
func TestRun工具_策略拒绝是IsError(t *testing.T) {
	impl := newTestServer(newTestSandbox(t))

	result, err := impl.handleRun(context.Background(), callRequest(map[string]any{
		"command": "shutdown -h now",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("被策略拒绝该回 IsError")
	}
	if body := resultText(t, result); !strings.Contains(body, "禁用列表") {
		t.Errorf("该说清是被禁用列表拦下：%s", body)
	}
}

// TestRun工具_缺身份默认拒绝 没带 `_meta` 身份、也没配默认租户 = fail-closed：回一句给
// 模型看的错误，不落进任何共享目录（多租户下最坏的失败形态）。
func TestRun工具_缺身份默认拒绝(t *testing.T) {
	impl := newTestServer(newTestSandbox(t))

	result, err := impl.handleRun(context.Background(), callRequestAs("", map[string]any{
		"command": `printf x`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("缺身份该回 IsError，而不是照常执行")
	}
	if body := resultText(t, result); !strings.Contains(body, "身份") {
		t.Errorf("该说清是缺调用方身份：%s", body)
	}
}

// TestRun工具_缺身份回落默认租户 配了 DefaultPrincipal 时，没带身份回落到那个**具名**
// 租户：既不是被拒、也不是所有匿名调用方共用一个共享根。
func TestRun工具_缺身份回落默认租户(t *testing.T) {
	requireBash(t)
	s := newTestSandbox(t)
	impl := &sandboxServer{
		opts:      Options{Root: s.Root, DefaultPrincipal: testPrincipal},
		sandboxes: map[string]*Sandbox{testPrincipal: s},
	}

	result, err := impl.handleRun(context.Background(), callRequestAs("", map[string]any{
		"command": `printf fallback-ok`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("配了默认租户就不该被拒：%s", resultText(t, result))
	}
	if body := resultText(t, result); !strings.Contains(body, "fallback-ok") {
		t.Errorf("该在那个默认租户的沙盒里跑：%s", body)
	}
}

// TestRun工具_显式身份压过默认租户 `_meta` 带了身份时就以它为准，默认租户只兜底。
func TestRun工具_显式身份压过默认租户(t *testing.T) {
	requireBash(t)
	s := newTestSandbox(t)
	other := newTestSandbox(t)
	impl := &sandboxServer{
		opts: Options{Root: s.Root, DefaultPrincipal: "fallback-tenant"},
		sandboxes: map[string]*Sandbox{
			testPrincipal:     s,
			"fallback-tenant": other,
		},
	}

	result, err := impl.handleRun(context.Background(), callRequestAs(testPrincipal, map[string]any{
		"command": `printf explicit-ok`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("带身份该照常执行：%s", resultText(t, result))
	}
	if body := resultText(t, result); !strings.Contains(body, "explicit-ok") {
		t.Errorf("该在带身份的租户沙盒里跑：%s", body)
	}
}

// TestRun说明_按是否允许pip动态生成 钉住两件事：装包那句随参数变化；无论哪一支，
// 都要提到能用 python/node 跑脚本、以及解释器缺失时是 command not found。
func TestRun说明_按是否允许pip动态生成(t *testing.T) {
	off := runDescription(false)
	if !strings.Contains(off, "装不了包") {
		t.Errorf("不允许 pip 时该说装不了包：%s", off)
	}
	on := runDescription(true)
	if !strings.Contains(on, "pip install <包>") {
		t.Errorf("允许 pip 时该给出可装包的说明：%s", on)
	}
	if off == on {
		t.Error("两个分支的说明不该一样")
	}

	for _, s := range []string{off, on} {
		for _, want := range []string{"python3", "node", "command not found"} {
			if !strings.Contains(s, want) {
				t.Errorf("run 工具说明里少了 %q：%s", want, s)
			}
		}
	}
}

// TestImport工具_把字节写进inbox import 是 agent→沙盒 的字节入口：agent 把本轮
// 入站图片的 base64 填进 `files`，工具写到本租户沙盒的 inbox/ 下并回报路径。
func TestImport工具_把字节写进inbox(t *testing.T) {
	s := newTestSandbox(t)
	impl := newTestServer(s)

	data := []byte("PNGDATA")
	result, err := impl.handleImport(context.Background(), callRequest(map[string]any{
		"files": []any{map[string]any{
			"name": "photo.png", "mime": "image/png",
			"data": base64.StdEncoding.EncodeToString(data),
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("导入该成功：%s", resultText(t, result))
	}

	got, err := os.ReadFile(filepath.Join(s.Root, "inbox", "photo.png"))
	if err != nil {
		t.Fatalf("文件没落进 inbox：%v", err)
	}
	if string(got) != "PNGDATA" {
		t.Errorf("内容不对：%q", got)
	}
	if body := resultText(t, result); !strings.Contains(body, "inbox/photo.png") {
		t.Errorf("结果该告诉模型路径：%s", body)
	}
}

// TestImport工具_空files要拒 没有 files（模型自己调、agent 没注入）时如实说一句，
// 不写空文件。
func TestImport工具_空files要拒(t *testing.T) {
	impl := newTestServer(newTestSandbox(t))
	result, err := impl.handleImport(context.Background(), callRequest(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("没有 files 该拒绝")
	}
}

// TestImport工具_重名自动让位 用户连发两张同名图不该互相覆盖。
func TestImport工具_重名自动让位(t *testing.T) {
	s := newTestSandbox(t)
	impl := newTestServer(s)

	call := func() {
		if _, err := impl.handleImport(context.Background(), callRequest(map[string]any{
			"files": []any{map[string]any{
				"name": "photo.png", "mime": "image/png",
				"data": base64.StdEncoding.EncodeToString([]byte("x")),
			}},
		})); err != nil {
			t.Fatal(err)
		}
	}
	call()
	call()

	entries, err := os.ReadDir(filepath.Join(s.Root, "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("两张同名图该各存一份，实际 %d 个文件", len(entries))
	}
}

// TestImportSkill工具_落脚本并保留子目录 技能脚本落进本租户 `skills/<技能>/`，
// 且**保留 `scripts/x.py` 这种子目录结构**（import 只取 basename，这里不能照抄）。
func TestImportSkill工具_落脚本并保留子目录(t *testing.T) {
	s := newTestSandbox(t)
	impl := newTestServer(s)

	result, err := impl.handleImportSkill(context.Background(), callRequest(map[string]any{
		"skill": "备份",
		"files": []any{
			map[string]any{"name": "scripts/run.py", "data": base64.StdEncoding.EncodeToString([]byte("print(1)\n"))},
			map[string]any{"name": "SKILL.md", "data": base64.StdEncoding.EncodeToString([]byte("步骤"))},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("落脚本该成功：%s", resultText(t, result))
	}

	ns := skillNamespace("备份")
	got, err := os.ReadFile(filepath.Join(s.Root, "skills", ns, "scripts", "run.py"))
	if err != nil {
		t.Fatalf("脚本没落进 skills/：%v", err)
	}
	if string(got) != "print(1)\n" {
		t.Errorf("内容不对：%q", got)
	}
	if body := resultText(t, result); !strings.Contains(body, "skills/"+ns+"/scripts/run.py") {
		t.Errorf("结果该返回相对沙盒根的可用路径：%s", body)
	}
}

// TestImportSkill工具_重名覆盖 技能是安装方维护的：同名脚本再落一次要覆盖，
// 不能像 import 那样攒出 `run-1.py`（否则改了脚本旧的还在）。
func TestImportSkill工具_重名覆盖(t *testing.T) {
	s := newTestSandbox(t)
	impl := newTestServer(s)

	call := func(body string) {
		if _, err := impl.handleImportSkill(context.Background(), callRequest(map[string]any{
			"skill": "备份",
			"files": []any{map[string]any{"name": "run.sh", "data": base64.StdEncoding.EncodeToString([]byte(body))}},
		})); err != nil {
			t.Fatal(err)
		}
	}
	call("v1")
	call("v2")

	ns := skillNamespace("备份")
	got, err := os.ReadFile(filepath.Join(s.Root, "skills", ns, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Errorf("重名该覆盖成最新：%q", got)
	}
	entries, err := os.ReadDir(filepath.Join(s.Root, "skills", ns))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("不该攒出多个同名文件：%v", entries)
	}
}

// TestImportSkill工具_路径越界要拒 脚本名可能带 `..`——不能让技能借它把文件写到
// 沙盒外面（或覆盖 SKILL.md 之外的路径）。
func TestImportSkill工具_路径越界要拒(t *testing.T) {
	s := newTestSandbox(t)
	impl := newTestServer(s)

	result, err := impl.handleImportSkill(context.Background(), callRequest(map[string]any{
		"skill": "备份",
		"files": []any{map[string]any{"name": "../evil.sh", "data": base64.StdEncoding.EncodeToString([]byte("x"))}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("越界路径该被拒")
	}
	if _, statErr := os.Stat(filepath.Join(s.Root, "evil.sh")); !os.IsNotExist(statErr) {
		t.Errorf("越界文件被写出来了：%v", statErr)
	}
}

// TestImportSkill工具_缺skill要拒 模型自己调、没给技能名时如实说一句。
func TestImportSkill工具_缺skill要拒(t *testing.T) {
	impl := newTestServer(newTestSandbox(t))
	result, err := impl.handleImportSkill(context.Background(), callRequest(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("缺 skill 该拒绝")
	}
}

// TestSkillNamespace_不同技能名不撞 safeSegment 会把中文压成一样的一串下划线，
// 单靠它「备份照片」与「照片备份」会落进同一个目录、互相覆盖脚本。短哈希要拉开。
func TestSkillNamespace_不同技能名不撞(t *testing.T) {
	a := skillNamespace("备份照片")
	b := skillNamespace("照片备份")
	if a == b {
		t.Errorf("不同技能名不该撞到同一个目录：%q == %q", a, b)
	}
	if again := skillNamespace("备份照片"); again != a {
		t.Errorf("同一技能名该稳定：%q != %q", again, a)
	}
}

// TestImport工具_inbox是软链要拒 写文件发生在 **server 进程**（不在 bwrap 内、有宿主
// 全部权限），而模型能经 run 造软链——写路径若只做词法检查，WriteFile 会顺着软链
// 把文件写到租户根外面。与 read 同一套双检必须也守写入。
func TestImport工具_inbox是软链要拒(t *testing.T) {
	s := newTestSandbox(t)
	impl := newTestServer(s)

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.Root, "inbox")); err != nil {
		t.Skipf("这个平台建不了软链：%v", err)
	}

	result, err := impl.handleImport(context.Background(), callRequest(map[string]any{
		"files": []any{map[string]any{
			"name": "photo.png", "mime": "image/png",
			"data": base64.StdEncoding.EncodeToString([]byte("x")),
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("inbox 是软链该拒绝：%s", resultText(t, result))
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("文件顺着软链写到了沙盒外：%v", entries)
	}
}

// TestImportSkill工具_技能目录是软链要拒 同一洞的 import_skill 版：skills/<命名空间>
// 被软链顶掉时，不但写入会逃逸，**MkdirAll 都会先在外面建目录**——所以双检要挡在
// 建目录之前。
func TestImportSkill工具_技能目录是软链要拒(t *testing.T) {
	s := newTestSandbox(t)
	impl := newTestServer(s)

	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(s.Root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.Root, "skills", skillNamespace("备份"))); err != nil {
		t.Skipf("这个平台建不了软链：%v", err)
	}

	result, err := impl.handleImportSkill(context.Background(), callRequest(map[string]any{
		"skill": "备份",
		"files": []any{map[string]any{"name": "run.sh", "data": base64.StdEncoding.EncodeToString([]byte("x"))}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("技能目录是软链该拒绝：%s", resultText(t, result))
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("目录/文件顺着软链跑到了沙盒外：%v", entries)
	}
}

// TestImportSkill工具_脚本的父目录是软链要拒 技能目录本身正常、但中间某级
// （如 scripts/）被换成软链——逐文件的双检要接住这一层。
func TestImportSkill工具_脚本的父目录是软链要拒(t *testing.T) {
	s := newTestSandbox(t)
	impl := newTestServer(s)

	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(s.Root, "skills", skillNamespace("备份")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.Root, "skills", skillNamespace("备份"), "scripts")); err != nil {
		t.Skipf("这个平台建不了软链：%v", err)
	}

	result, err := impl.handleImportSkill(context.Background(), callRequest(map[string]any{
		"skill": "备份",
		"files": []any{map[string]any{"name": "scripts/run.py", "data": base64.StdEncoding.EncodeToString([]byte("x"))}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("父目录是软链该拒绝：%s", resultText(t, result))
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("脚本顺着软链写到了沙盒外：%v", entries)
	}
}
