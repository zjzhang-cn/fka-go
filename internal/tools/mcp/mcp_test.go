package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// write 造一份 mcp.json 并把 FKA_MCP_CONFIG 指过去。
func write(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写临时配置失败：%v", err)
	}
	t.Setenv("FKA_MCP_CONFIG", path)
}

func TestReadConfig_文件不存在是正常状态(t *testing.T) {
	t.Setenv("FKA_MCP_CONFIG", filepath.Join(t.TempDir(), "没有这个文件.json"))

	read, err := ReadConfig()
	if err != nil {
		t.Fatalf("文件不存在不该是错误：%v", err)
	}
	if read != nil {
		t.Errorf("应返回 nil（没有 MCP）：%+v", read)
	}
}

func TestReadConfig_stdio与http两种形状(t *testing.T) {
	write(t, `{
	  "mcpServers": {
	    "filesystem": { "command": "npx", "args": ["-y", "server", "/mnt/nas"], "env": { "A": "1" } },
	    "remote": { "url": "https://example.com/mcp", "headers": { "Authorization": "Bearer x" } }
	  }
	}`)

	read, err := ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig 返错：%v", err)
	}
	if len(read.Servers) != 2 {
		t.Fatalf("应有 2 个服务器：%+v", read.Servers)
	}

	fs := read.Servers["filesystem"]
	if fs.Command != "npx" || len(fs.Args) != 3 || fs.Env["A"] != "1" {
		t.Errorf("stdio 形状解析不对：%+v", fs)
	}
	if fs.URL != "" {
		t.Errorf("stdio 服务器不该有 url：%+v", fs)
	}

	remote := read.Servers["remote"]
	if remote.URL != "https://example.com/mcp" || remote.Headers["Authorization"] != "Bearer x" {
		t.Errorf("http 形状解析不对：%+v", remote)
	}
	if remote.Command != "" {
		t.Errorf("http 服务器不该有 command：%+v", remote)
	}
}

func TestReadConfig_没有mcpServers键算空(t *testing.T) {
	write(t, `{"别的": 1}`)

	read, err := ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig 返错：%v", err)
	}
	if read == nil || len(read.Servers) != 0 {
		t.Errorf("应是空配置：%+v", read)
	}
}

// TestReadConfig_格式错要报错 「配了但没生效」是最难查的一类，配置错必须显式。
func TestReadConfig_格式错要报错(t *testing.T) {
	cases := map[string]string{
		"不是 JSON":           `{ 坏掉的`,
		"顶层不是对象":            `[]`,
		"mcpServers 不是对象":   `{"mcpServers": []}`,
		"服务器不是对象":           `{"mcpServers": {"a": 1}}`,
		"既没 command 也没 url": `{"mcpServers": {"a": {}}}`,
		"args 不是字符串数组":      `{"mcpServers": {"a": {"command": "x", "args": [1]}}}`,
		"env 不是对象":          `{"mcpServers": {"a": {"command": "x", "env": "y"}}}`,
		"env 的值不是字符串":       `{"mcpServers": {"a": {"command": "x", "env": {"K": 1}}}}`,
		"headers 不是对象":      `{"mcpServers": {"a": {"url": "u", "headers": 1}}}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			write(t, body)
			_, err := ReadConfig()
			if err == nil {
				t.Fatalf("该报配置错")
			}
			if _, ok := AsConfigError(err); !ok {
				t.Errorf("应是 *ConfigError，调用方据此决定跳过还是拒绝启动：%T", err)
			}
		})
	}
}

func TestResolveConfigPath_相对路径按安装根(t *testing.T) {
	t.Setenv("FKA_MCP_CONFIG", "conf/mcp.json")
	// `fka` 是全局命令，cwd 是任意的——相对路径必须按安装根解析
	if got := ResolveConfigPath(); got != filepath.Join(homeForTest(), "conf", "mcp.json") {
		t.Errorf("= %q", got)
	}

	abs := filepath.Join(t.TempDir(), "x.json")
	t.Setenv("FKA_MCP_CONFIG", abs)
	if got := ResolveConfigPath(); got != abs {
		t.Errorf("绝对路径应原样使用：%q", got)
	}
}

func TestSanitizeServerKey(t *testing.T) {
	cases := map[string]string{
		"sqlite":       "sqlite",
		"  My-Server ": "my_server",
		"time__demo":   "time__demo", // 下划线本来就合法
		"!!!":          "",
	}
	for input, want := range cases {
		if got := sanitizeServerKey(input); got != want {
			t.Errorf("sanitizeServerKey(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestContentToText_未知类型不假装是文本(t *testing.T) {
	blocks := []mcpContent{
		{Type: "text", Text: "第一段"},
		{Type: "image", MimeType: "image/png"},
		{Type: "resource", Resource: struct {
			URI  string
			Text string
		}{URI: "fka://files/a.pdf", Text: "PDF 正文"}},
		{Type: "resource", Resource: struct {
			URI  string
			Text string
		}{URI: "fka://files/b.pdf"}},
		{Type: "别的东西", Raw: `{"type":"未来类型"}`},
	}

	got := contentToText(blocks)
	for _, want := range []string{"第一段", "[图片 image/png]", "PDF 正文", "[资源 fka://files/b.pdf]", "未来类型"} {
		if !contains(got, want) {
			t.Errorf("缺 %q：\n%s", want, got)
		}
	}
}

func TestFallbackText(t *testing.T) {
	if got := fallbackText(""); got != "（工具没有返回内容）" {
		t.Errorf("= %q", got)
	}
	if got := fallbackText("有内容"); got != "有内容" {
		t.Errorf("= %q", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// homeForTest 取安装根（测试里只用来验证「相对路径按安装根解析」）。
func homeForTest() string { return config.Home() }
