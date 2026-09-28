package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
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
}

// HTTPServer 远程（streamable HTTP）服务器。
type HTTPServer struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
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
		return cfg, nil
	}

	return ServerConfig{}, newConfigError(
		"mcp.json 里服务器 %s 既没有 command（stdio）也没有 url（HTTP）：%s", name, path)
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
