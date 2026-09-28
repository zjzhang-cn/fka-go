// Package config 放项目文件的位置与环境变量的加载。
//
// ## 为什么默认路径不能相对 cwd
//
// `fka` 是**全局命令**——装好之后用户会在任意目录下调用它。而 .env、socket、状态快照、
// 日志这些都属于**这一份安装**，不属于调用者的当前目录。写成 `./data/fka.sock` 时，
// 从 ~ 下跑 `fka channel status` 会去读 `~/data/account-status.json`——读不到就静默显示
// 「未配置任何 iLink 账号」，看着像配置丢了，实际是路径找错了地方。
//
// 所以所有默认路径都相对**安装根**解析。Go 版把安装根定义成：
//
//	$FKA_HOME            —— 显式指定，压倒一切（部署与测试都靠它）
//	可执行文件所在目录      —— 静态二进制的天然锚点，跨机器拷贝后仍然自洽
//	当前工作目录          —— 前两者都拿不到时的兜底，至少行为可预期
//
// 环境变量仍然可以覆盖具体路径，那是显式选择，与「碰巧 cwd 不对」不是一回事。
package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	rootOnce sync.Once
	rootPath string
)

// Home 返回安装根。**在进程内只解析一次**——它是全局不变量，反复解析
// 会在测试里给出不一致的结果（改了 cwd 或环境变量之后）。
func Home() string {
	rootOnce.Do(func() {
		rootPath = resolveHome()
	})
	return rootPath
}

// SetHome 覆盖安装根。**只给测试与嵌入式用法**：CLI 入口之外不该调用它。
func SetHome(path string) {
	rootOnce.Do(func() {})
	rootPath = path
}

func resolveHome() string {
	if h := strings.TrimSpace(os.Getenv("FKA_HOME")); h != "" {
		return h
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		return filepath.Dir(exe)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// DataPath 返回 data/ 下的路径。**不要**用相对路径拼这里的东西。
func DataPath(name string) string {
	return filepath.Join(Home(), "data", name)
}

// LogDir 返回日志目录。
//
// **相对路径按安装根解析，不按 cwd**——服务是常驻进程，相对 cwd 的日志目录会因为
// 启动位置不同而跑到别处；而「日志去哪了」是最不该需要排查的事。
func LogDir() string {
	configured := os.Getenv("LOG_DIR")
	if configured == "" {
		return filepath.Join(Home(), "logs")
	}
	if filepath.IsAbs(configured) {
		return configured
	}
	return filepath.Join(Home(), configured)
}

// EnvPath 返回 .env 的位置。**永远是安装根下那一份**：服务可能在任意 cwd 下运行，
// 而 LoadEnv 只读这个文件。
func EnvPath() string { return filepath.Join(Home(), ".env") }

// LoadEnv 加载 <安装根>/.env。
//
// 已存在的环境变量**不会被覆盖**，所以显式设置仍然优先——这与 dotenv 的默认行为一致，
// 也是「FKA_HOME 之外还能临时改一个值」的前提。
func LoadEnv() {
	_, _ = LoadEnvFrom(EnvPath())
}

// LoadEnvFrom 从指定文件加载，返回解析出的条目数。文件不存在不是错误。
func LoadEnvFrom(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	count := 0
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, unquoteDotenv(strings.TrimSpace(line[eq+1:]))); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// unquoteDotenv 去掉 dotenv 的引号包裹。
//
// 双引号里处理 \n 与 \t 这两个实际会遇到的转义；单引号按 shell 惯例原样取内容。
func unquoteDotenv(value string) string {
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		inner := value[1 : len(value)-1]
		replacer := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r", `\"`, `"`, `\\`, `\`)
		return replacer.Replace(inner)
	}
	// 无引号：行尾注释不是 dotenv 语法的一部分，保留原样
	return value
}
