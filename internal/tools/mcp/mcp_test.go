package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
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
		"transport 不是字符串":   `{"mcpServers": {"a": {"url": "u", "transport": 1}}}`,
		"transport 值不认识":    `{"mcpServers": {"a": {"url": "u", "transport": "websocket"}}}`,
		"cwd 不是字符串":         `{"mcpServers": {"a": {"command": "x", "cwd": 1}}}`,
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

func TestReadConfig_cwd相对路径按安装根(t *testing.T) {
	dir := t.TempDir()
	write(t, `{"mcpServers": {
	  "rel":  { "command": "x", "cwd": "servers/memory" },
	  "abs":  { "command": "x", "cwd": "`+dir+`" },
	  "none": { "command": "x" }
	}}`)

	read, err := ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig 返错：%v", err)
	}

	// fka 是全局命令，cwd 是任意的：工作目录不按 cwd 解析，否则同一份
	// mcp.json 从不同目录起会落在不同的盘上
	if got, want := read.Servers["rel"].Cwd, filepath.Join(homeForTest(), "servers", "memory"); got != want {
		t.Errorf("相对 cwd = %q，期望 %q", got, want)
	}
	if got := read.Servers["abs"].Cwd; got != dir {
		t.Errorf("绝对 cwd 应原样使用：%q", got)
	}
	if got := read.Servers["none"].Cwd; got != "" {
		t.Errorf("没配 cwd 应为空（继承 fka 的）：%q", got)
	}
}

// HTTP 服务器没有子进程可设工作目录：忽略即可，但不能让整个 mcp.json 作废。
func TestReadConfig_http服务器的cwd被忽略而不报错(t *testing.T) {
	write(t, `{"mcpServers": {"remote": {"url": "https://example.com/mcp", "cwd": "somewhere"}}}`)

	read, err := ReadConfig()
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got := read.Servers["remote"].Cwd; got != "" {
		t.Errorf("HTTP 服务器不该带 cwd：%q", got)
	}
}

// TestStdioCommand_只改工作目录 别的地方（env 等）一旦跟着变，就是「为了 cwd
// 修好了命令、弄坏了密钥」——那种 bug 要等到线上才看得出来。
func TestStdioCommand_只改工作目录(t *testing.T) {
	dir := t.TempDir()

	cmd, err := stdioCommand(dir)(context.Background(), "some-server", []string{"K=V"}, []string{"--flag"})
	if err != nil {
		t.Fatalf("造命令返错：%v", err)
	}
	if cmd.Dir != dir {
		t.Errorf("Dir = %q，期望 %q", cmd.Dir, dir)
	}
	if !slices.Contains(cmd.Env, "K=V") {
		t.Errorf("mcp.json 里的 env 要透传：%v", cmd.Env)
	}
	if len(cmd.Args) != 2 || cmd.Args[1] != "--flag" {
		t.Errorf("args 要原样透传：%v", cmd.Args)
	}
}

func TestNewClient_工作目录不可用要说清是哪个服务器(t *testing.T) {
	cfg := ServerConfig{Command: "some-server", Cwd: filepath.Join(t.TempDir(), "没有这个目录")}

	_, err := newClient(context.Background(), cfg)
	if err == nil {
		t.Fatalf("目录不存在该报错")
	}
	// 命令名必须出现在消息里：一次要起好几个 server，报「起 x 的连接失败」
	// 而没说 x 是谁，就等于让用户去翻日志猜
	for _, want := range []string{"some-server", "工作目录"} {
		if !contains(err.Error(), want) {
			t.Errorf("报错里应说明是哪个服务器的什么问题：%s", err.Error())
		}
	}
}

func TestNewClient_工作目录是个文件也算不可用(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("造临时文件失败：%v", err)
	}

	if _, err := newClient(context.Background(), ServerConfig{Command: "some-server", Cwd: file}); err == nil {
		t.Fatalf("指向文件该报错")
	}
}

// HTTP 的两种传输不能互相顶替，而**选错的报错与真实原因毫无关系**
// （404 session terminated / transport not started yet），所以猜的那条规则
// 与「显式压过自动」都要钉住。
func TestPickTransport_显式压过自动(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
		want string
	}{
		{"显式 sse 压过 /mcp", ServerConfig{URL: "https://h/mcp", Transport: transportSSE}, transportSSE},
		{"显式 http 压过 /sse", ServerConfig{URL: "https://h/sse", Transport: transportHTTP}, transportHTTP},
		{"/sse 结尾", ServerConfig{URL: "https://h/sse"}, transportSSE},
		{"/sse 带查询串", ServerConfig{URL: "https://h/sse?token=x"}, transportSSE},
		{"/sse 带尾斜杠", ServerConfig{URL: "https://h/sse/"}, transportSSE},
		{"大写的 /SSE", ServerConfig{URL: "https://h/SSE"}, transportSSE},
		{"路径中间出现 sse 不算", ServerConfig{URL: "https://h/sse-tools/mcp"}, transportHTTP},
		{"/mcp", ServerConfig{URL: "https://h/mcp"}, transportHTTP},
		{"url 解析不了就当 http", ServerConfig{URL: "://坏地址"}, transportHTTP},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickTransport(c.cfg); got != c.want {
				t.Errorf("= %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestReadConfig_transport只认两个值 认不出的值**直接报错**：传输选错的表现
// 是握手失败，而配置读回来是完整的——「配了但没生效」里最难查的一种。
func TestReadConfig_transport只认两个值(t *testing.T) {
	write(t, `{"mcpServers": {
	  "说清楚": { "url": "https://h/sse", "transport": "SSE" },
	  "没给":   { "url": "https://h/sse" }
	}}`)

	read, err := ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig 返错：%v", err)
	}
	if got := read.Servers["说清楚"].Transport; got != transportSSE {
		t.Errorf("大小写该归一，实际 %q", got)
	}
	if got := read.Servers["没给"].Transport; got != "" {
		t.Errorf("没给该留空（由 pickTransport 猜），实际 %q", got)
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
		{Type: "audio", MimeType: "audio/wav"},
		{Type: "resource", Resource: mcpResource{URI: "fka://files/a.pdf", Text: "PDF 正文"}},
		{Type: "resource", Resource: mcpResource{URI: "fka://files/b.pdf", MimeType: "application/pdf"}},
		{Type: "resource_link", URI: "fka://files/c.pdf", Name: "c.pdf"},
		{Type: "别的东西", Raw: `{"type":"未来类型"}`},
	}

	got := contentToText(blocks)
	for _, want := range []string{
		"第一段", "[图片 image/png]", "[音频 audio/wav",
		"PDF 正文", "[资源 fka://files/b.pdf application/pdf]",
		"[资源链接 c.pdf fka://files/c.pdf]", "未来类型",
	} {
		if !contains(got, want) {
			t.Errorf("缺 %q：\n%s", want, got)
		}
	}
}

// TestImagesFromContent_图片抽成附件且超限跳过 image 块要转成 data URI 附件；超过
// 总上限的图**跳过而不是把上下文撑爆**。
func TestImagesFromContent_图片抽成附件且超限跳过(t *testing.T) {
	small := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	images := imagesFromContent([]mcpContent{
		{Type: "text", Text: "说明"},
		{Type: "image", MimeType: "image/png", Data: small},
	})
	if len(images) != 1 {
		t.Fatalf("该抽出 1 张图，实际 %d", len(images))
	}
	if want := "data:image/png;base64," + small; images[0].DataURI != want {
		t.Errorf("DataURI = %q，期望 %q", images[0].DataURI, want)
	}

	// 嵌入式图片资源（type=resource，blob 且 mime 是 image/*）也要抽出来
	embedded := imagesFromContent([]mcpContent{
		{Type: "resource", Resource: mcpResource{MimeType: "image/jpeg", Blob: small}},
	})
	if len(embedded) != 1 || embedded[0].DataURI != "data:image/jpeg;base64,"+small {
		t.Errorf("嵌入式图片资源该被抽出：%+v", embedded)
	}

	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), maxToolImageBytes+1))
	if got := imagesFromContent([]mcpContent{{Type: "image", MimeType: "image/png", Data: big}}); len(got) != 0 {
		t.Errorf("超过上限的图该跳过，实际抽出 %d 张", len(got))
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
