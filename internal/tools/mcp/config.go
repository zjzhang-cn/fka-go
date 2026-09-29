package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// ConfigError mcp.json 读不了。Hint 说明下一步能做什么。
type ConfigError struct {
	Message string
	Hint    string
}

func (e *ConfigError) Error() string { return e.Message }

func newConfigError(format string, args ...any) *ConfigError {
	return &ConfigError{Message: fmt.Sprintf(format, args...)}
}

// StdioServer 本地进程（stdio）服务器。
type StdioServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
}

// HTTPServer 远程服务器。**HTTP 有两种传输**，不是一种：
//
//   - `sse`（老）：GET 开着一条流，服务端先给一个 `endpoint` 事件告诉你往哪 POST，
//     POST 只回 `202 Accepted`，**真正的响应从那条流上回来**；
//   - `http`（streamable，新）：直接 POST 那个 url，响应就在响应体里。
//
// 两者**不能互相顶替**：拿新的客户端去 POST 一个 `/sse` 地址，拿到的是
// `404 session terminated` —— 而配置看上去完全正确。
type HTTPServer struct {
	URL string `json:"url"`
	// Transport `sse` / `http`。**空 = 按 url 的形状自己猜**（见 pickTransport）
	Transport string            `json:"transport,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// ServerConfig 一个服务器的配置。**不是联合类型**：Go 里用
// `Command != ""` 判 stdio、`URL != ""` 判 HTTP，比 interface 判别省一层间接，
// 也让「两个都填」这种配置错误能被显式报出来。
type ServerConfig struct {
	Command string
	Args    []string
	Env     map[string]string
	URL     string
	Headers map[string]string
	// Transport HTTP 服务器用哪种传输（`sse` / `http`）。空 = 猜。
	// **只对 url 有意义**，stdio 忽略它（它自己就是本地管道）
	Transport string
	// Cwd 子进程的工作目录。**空 = 继承 fka 自己的**（于是跟着「谁在哪个
	// 目录敲的 fka」变，见 resolveCwd）；对 HTTP 服务器无意义
	Cwd string
}

// describe 人看的名字，给错误信息用。
func (c ServerConfig) describe() string {
	if c.Command != "" {
		return c.Command
	}
	if c.URL != "" {
		return c.URL
	}
	return "(未配置)"
}

// Config 一份读好的 mcp.json。
type Config struct {
	// Path 实际读到的文件路径，日志与 `fka tools` 都要说清楚
	Path    string
	Servers map[string]ServerConfig
}

// ResolveConfigPath 配置路径。FKA_MCP_CONFIG 可覆盖；相对路径**按安装根**解析，
// 不按 cwd——与 .env、技能目录同一条理由（fka 是全局命令，cwd 是任意的）。
func ResolveConfigPath() string {
	configured := strings.TrimSpace(os.Getenv("FKA_MCP_CONFIG"))
	if configured == "" {
		return filepath.Join(config.Home(), "mcp.json")
	}
	if filepath.IsAbs(configured) {
		return configured
	}
	return filepath.Join(config.Home(), configured)
}

// ReadConfig 读配置。文件不存在返回 nil（没有 MCP，正常）；格式错返 *ConfigError。
//
// ## 格式：标准 mcpServers
//
// 与 Claude Desktop 等客户端同一套形状，方便直接抄现成配置：
//
//	{
//	  "mcpServers": {
//	    "filesystem": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/mnt/nas"] },
//	    "remote": { "url": "https://example.com/mcp", "headers": { "Authorization": "Bearer …" } }
//	  }
//	}
//
// 有 command 的是 **stdio**（本地起进程），有 url 的是 **streamable HTTP**。
// stdio 还可以给 `cwd` 指定子进程的工作目录（相对路径按安装根解析）。
//
// ## 文件不存在 = 没有 MCP，是正常状态
//
// 与技能目录同一条：没配不是错，不告警。**格式错**才是错——那会让用户以为
// 「配了但没生效」，所以这里返 ConfigError，由调用方决定是拒绝启动还是跳过
// （本项目的选择是记错误日志后跳过，不让一个坏配置拖垮服务）。
func ReadConfig() (*Config, error) {
	path := ResolveConfigPath()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, newConfigError("mcp.json 读不了：%s", path)
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, &ConfigError{
			Message: "mcp.json 不是合法 JSON：" + path,
			Hint:    err.Error(),
		}
	}

	top, ok := parsed.(map[string]any)
	if !ok {
		return nil, newConfigError("mcp.json 顶层必须是对象：%s", path)
	}

	rawServers, present := top["mcpServers"]
	if !present {
		return &Config{Path: path, Servers: map[string]ServerConfig{}}, nil
	}

	serversMap, ok := rawServers.(map[string]any)
	if !ok {
		return nil, newConfigError("mcp.json 的 mcpServers 必须是对象：%s", path)
	}

	servers := make(map[string]ServerConfig, len(serversMap))
	for name, value := range serversMap {
		cfg, err := parseServer(name, value, path)
		if err != nil {
			return nil, err
		}
		servers[name] = cfg
	}

	return &Config{Path: path, Servers: servers}, nil
}

func parseServer(name string, value any, path string) (ServerConfig, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return ServerConfig{}, newConfigError("mcp.json 里服务器 %s 的配置必须是对象：%s", name, path)
	}

	command, hasCommand := raw["command"].(string)
	if hasCommand && command != "" {
		cfg := ServerConfig{Command: command}

		if rawArgs, present := raw["args"]; present && rawArgs != nil {
			list, ok := rawArgs.([]any)
			if !ok {
				return ServerConfig{}, newConfigError(
					"mcp.json 里服务器 %s 的 args 必须是字符串数组：%s", name, path)
			}
			for _, item := range list {
				text, ok := item.(string)
				if !ok {
					return ServerConfig{}, newConfigError(
						"mcp.json 里服务器 %s 的 args 必须是字符串数组：%s", name, path)
				}
				cfg.Args = append(cfg.Args, text)
			}
		}

		if rawEnv, present := raw["env"]; present && rawEnv != nil {
			env, err := stringRecord(name, "env", rawEnv, path)
			if err != nil {
				return ServerConfig{}, err
			}
			cfg.Env = env
		}

		if rawCwd, present := raw["cwd"]; present && rawCwd != nil {
			text, ok := rawCwd.(string)
			if !ok {
				return ServerConfig{}, newConfigError(
					"mcp.json 里服务器 %s 的 cwd 必须是字符串：%s", name, path)
			}
			cfg.Cwd = resolveCwd(text)
		}

		return cfg, nil
	}

	url, hasURL := raw["url"].(string)
	if hasURL && url != "" {
		cfg := ServerConfig{URL: url}
		if rawHeaders, present := raw["headers"]; present && rawHeaders != nil {
			headers, err := stringRecord(name, "headers", rawHeaders, path)
			if err != nil {
				return ServerConfig{}, err
			}
			cfg.Headers = headers
		}
		if rawTransport, present := raw["transport"]; present && rawTransport != nil {
			// **认不出的值直接报错**：传输选错的表现是握手 404，而配置看上去
			// 完全正常——那正是「配了但没生效」最难查的一种。宁可启动时就说清
			transport, ok := rawTransport.(string)
			if !ok {
				return ServerConfig{}, newConfigError(
					"mcp.json 里服务器 %s 的 transport 必须是字符串：%s", name, path)
			}
			switch strings.ToLower(strings.TrimSpace(transport)) {
			case "":
			case transportSSE, transportHTTP:
				cfg.Transport = strings.ToLower(strings.TrimSpace(transport))
			default:
				return ServerConfig{}, newConfigError(
					"mcp.json 里服务器 %s 的 transport 只能是 %s 或 %s，收到 %q：%s",
					name, transportSSE, transportHTTP, transport, path)
			}
		}
		// HTTP 服务器没有子进程，cwd 无处可去。**忽略而不是报错**：
		// 报错会让整个 mcp.json 作废、把别的服务器一起带走，而忽略没有运行时代价。
		// 但必须留一条痕迹——否则用户以为「配了工作目录」而其实没有
		if rawCwd, present := raw["cwd"]; present && rawCwd != nil {
			config.Log().Warn(config.TypeSYS, "mcp.json 里 HTTP 服务器的 cwd 只对 stdio 有意义，已忽略",
				config.Context{"server": name, "path": path})
		}
		return cfg, nil
	}

	return ServerConfig{}, newConfigError(
		"mcp.json 里服务器 %s 既没有 command（stdio）也没有 url（HTTP）：%s", name, path)
}

// HTTP 的两种传输。**名字与 mcp.json 里的 `transport` 取值一字不差**——
// 写错一个字符的表现是「配置读进来了但没人认」。
const (
	transportSSE  = "sse"
	transportHTTP = "http"
)

// pickTransport 这个 HTTP 服务器该用哪种传输。
//
// ## 显式优先，其次看 url 的形状
//
// 猜的依据只有一条：**path 以 `/sse` 结尾**（去掉 query 与 fragment 再看）。
// 那是约定而不是保证——所以它只是兜底，并且**猜出来的结果要进日志**。
//
// ## 为什么不给「先试一种，失败再换一种」
//
// 看着更聪明，实际更糟：streamable 的失败原因五花八门（401、404、超时、DNS），
// 只有一部分说明「这是台老服务器」。为了让那一小类能落到 SSE 上，得给错误分类，
// 而分类判错时的表现是**两段都试过、两段都失败**，用户看到的是一条比原来更长的
// 错误信息，却拿不到「该写什么」的建议。不如猜错时说清怎么写。
func pickTransport(cfg ServerConfig) string {
	if cfg.Transport != "" {
		return cfg.Transport
	}
	// 解析失败就当不是 SSE：url 反过来由 SDK 报错，那条消息更具体
	parsed, err := url.Parse(cfg.URL)
	if err != nil {
		return transportHTTP
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/"+transportSSE) {
		return transportSSE
	}
	return transportHTTP
}

// resolveCwd 把配置里的工作目录变成一个可直接交给 exec 的路径。
//
// **相对路径按安装根解析**，不按 cwd——与 ResolveConfigPath 同一条理由。
// 这里尤其重要：不解析的话子进程会继承「谁在哪个目录敲的 fka」，而 `fka` 是
// 全局命令、从 Makefile 起、从终端起、从 launchd 起各是不同目录，同一份
// mcp.json 每次跑在不同的盘上。**目录不存在不在这时报错**（那是连接期的事，
// 一个服务器连不上不该让整个 mcp.json 作废），由 client 在起进程前查。
func resolveCwd(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if filepath.IsAbs(trimmed) {
		return trimmed
	}
	return filepath.Join(config.Home(), trimmed)
}

func stringRecord(server, field string, value any, path string) (map[string]string, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, newConfigError("mcp.json 里服务器 %s 的 %s 必须是对象：%s", server, field, path)
	}

	out := make(map[string]string, len(raw))
	for key, item := range raw {
		text, ok := item.(string)
		if !ok {
			return nil, newConfigError(
				"mcp.json 里服务器 %s 的 %s.%s 必须是字符串：%s", server, field, key, path)
		}
		out[key] = text
	}
	return out, nil
}

// AsConfigError 判断一个错误是不是配置错误。调用方用它决定「跳过」还是「拒绝启动」。
func AsConfigError(err error) (*ConfigError, bool) {
	var target *ConfigError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
