package mcp

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

var serverKeyIllegal = regexp.MustCompile(`[^a-z0-9_]`)

// sanitizeServerKey 服务器名里的非法字符换成 _，与工具名的分隔 __ 配套。
func sanitizeServerKey(raw string) string {
	cleaned := strings.ToLower(strings.TrimSpace(raw))
	cleaned = serverKeyIllegal.ReplaceAllString(cleaned, "_")
	return strings.Trim(cleaned, "_")
}

type connectedServer struct {
	// name 配置里的原始名字，日志用
	name       string
	connection Connection
}

// SourceOptions 造源需要的依赖。
type SourceOptions struct {
	Servers map[string]ServerConfig
	// Connect 连接方式。nil = 真连；测试注入假的
	Connect ConnectFunc
}

// source 聚合源。
type source struct {
	connect    ConnectFunc
	configured map[string]ServerConfig

	mu    sync.Mutex
	ready bool
	specs []tools.Spec
	// toolIndex 短名 → 谁提供它。Call 靠它定位，不用去解析字符串
	toolIndex map[string]toolLocator
	// connected 键是归一化后的短名
	connected map[string]*connectedServer
}

type toolLocator struct {
	serverKey string
	toolName  string
}

// NewSource 造 MCP 工具源。
func NewSource(options SourceOptions) tools.Source {
	connect := options.Connect
	if connect == nil {
		connect = ConnectMcpServer
	}
	configured := options.Servers
	if configured == nil {
		configured = map[string]ServerConfig{}
	}
	return &source{
		connect:    connect,
		configured: configured,
		toolIndex:  map[string]toolLocator{},
		connected:  map[string]*connectedServer{},
	}
}

func (s *source) ID() string    { return "mcp" }
func (s *source) Label() string { return "MCP（外部工具）" }

func (s *source) List(ctx context.Context, tc tools.Context) ([]tools.Spec, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tools.Spec(nil), s.specs...), nil
}

func (s *source) Call(ctx context.Context, name string, args map[string]any, tc tools.Context) (tools.Result, error) {
	if err := s.ensure(ctx); err != nil {
		return tools.Result{}, err
	}

	s.mu.Lock()
	locator, known := s.toolIndex[name]
	server := s.connected[locator.serverKey]
	s.mu.Unlock()

	if !known {
		return tools.FailResult("没有叫 %s 的 MCP 工具。", name), nil
	}
	if server == nil {
		return tools.FailResult("MCP 服务器 %s 当前不可用。", locator.serverKey), nil
	}

	result, err := server.connection.CallTool(ctx, locator.toolName, args)
	if err != nil {
		config.Log().Warn(config.TypeTOOL, "MCP 工具调用失败",
			config.Context{"tool": name, "error": err.Error()})
		return tools.FailResult("MCP 工具调用失败：%s", err.Error()), nil
	}

	return tools.Result{OK: result.OK, Content: result.Content}, nil
}

// PromptSection 不产出额外段落。
//
// 刻意不加「当前挂了哪些 MCP 服务器」这类清单：**服务器名是配置里的内容，
// 让它进 system prompt 就等于给了一份可被文档内容影响的开销表**。模型要什么工具
// 看 tools 数组即可。
func (s *source) PromptSection(ctx tools.Context) (string, error) { return "", nil }

func (s *source) Close() error {
	s.mu.Lock()
	servers := make([]*connectedServer, 0, len(s.connected))
	for _, server := range s.connected {
		servers = append(servers, server)
	}
	s.connected = map[string]*connectedServer{}
	s.mu.Unlock()

	// stdio 服务器是被我们拉起的子进程，不关就留在后台
	for _, server := range servers {
		if err := server.connection.Close(); err != nil {
			config.Log().Warn(config.TypeSYS, "MCP 连接关闭失败",
				config.Context{"server": server.name, "error": err.Error()})
		}
	}
	return nil
}

// ensure 惰性连接：第一次列工具时才连，连一次后缓存。
func (s *source) ensure(ctx context.Context) error {
	s.mu.Lock()
	if s.ready {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// 顺序固定：map 遍历是随机的，而**服务器连上的顺序会进日志**。
	// 随机顺序让「同一个配置两次启动日志不一样」，多账号排查时很误导
	names := make([]string, 0, len(s.connectServers()))
	for name := range s.connectServers() {
		names = append(names, name)
	}
	sort.Strings(names)

	// **并行连**：N 个服务器串行等握手，N×20s 的启动延迟没人受得了
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			s.connectOne(ctx, name)
		}(name)
	}
	wg.Wait()

	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	return nil
}

// connectServers 拿配置快照。加这把锁是为了 ensure 的并行分支读到的 map
// 不与调用方后续改配置并发。
func (s *source) connectServers() map[string]ServerConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configured == nil {
		return map[string]ServerConfig{}
	}
	return s.configured
}

func (s *source) connectOne(ctx context.Context, name string) {
	servers := s.connectServers()
	cfg, ok := servers[name]
	if !ok {
		return
	}

	key := sanitizeServerKey(name)
	if key == "" {
		config.Log().Warn(config.TypeSYS, "MCP 服务器名归一化后为空，已跳过", config.Context{"server": name})
		return
	}

	s.mu.Lock()
	_, duplicate := s.connected[key]
	s.mu.Unlock()
	if duplicate {
		config.Log().Warn(config.TypeSYS, "MCP 服务器名归一化后重复，后一个跳过",
			config.Context{"server": name, "key": key})
		return
	}

	connection, err := s.connect(ctx, name, cfg)
	if err != nil {
		// 一个服务器连不上不能把别的带走——少一批工具，服务照常
		config.Log().Warn(config.TypeSYS, "MCP 服务器连接失败，已跳过",
			config.Context{"server": name, "error": err.Error()})
		return
	}

	listed, err := connection.ListTools(ctx)
	if err != nil {
		_ = connection.Close()
		config.Log().Warn(config.TypeSYS, "MCP 服务器列举工具失败，已跳过",
			config.Context{"server": name, "error": err.Error()})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.connected[key]; exists {
		// 并行连时两个配置项归一化到同一个 key：后到的让位，并关掉多余的连接，
		// 否则那个子进程会一直挂着
		go func() { _ = connection.Close() }()
		return
	}
	s.connected[key] = &connectedServer{name: name, connection: connection}

	for _, tool := range listed {
		shortName := key + tools.ToolNameSeparator + tool.Name
		if _, exists := s.toolIndex[shortName]; exists {
			config.Log().Warn(config.TypeSYS, "MCP 工具名重复，后一个跳过",
				config.Context{"tool": shortName, "server": name})
			continue
		}
		s.toolIndex[shortName] = toolLocator{serverKey: key, toolName: tool.Name}
		s.specs = append(s.specs, tools.Spec{
			Name:        shortName,
			Description: tool.Description,
			Parameters:  tool.InputSchema,
			// 一律 external：MCP 会出网或拉起别的进程
			Effect: tools.EffectExternal,
		})
	}

	config.Log().Info(config.TypeSYS, "MCP 服务器已连接："+name, config.Context{"tools": len(listed)})
}
