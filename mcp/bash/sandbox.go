// 沙盒：把模型给的命令放进一个**有真实边界**的环境里跑。
//
// ## 两档模式
//
//   - **bwrap（默认）**：用 Bubblewrap 起一个 Linux 命名空间沙盒。这是真正的
//     OS 级隔离：**文件系统的写权限只剩沙盒根**（`/` 只读绑定），网络、PID、IPC、
//     UTS、user 命名空间全部另起。`cd /` 之类逃不出写边界，因为它根本不可写。
//     代价是要求系统装了 `bwrap`，且只在 Linux 上有意义。
//   - **direct（退化档）**：纯 Go、零依赖，只把 cwd 固定住再直接起 `bash`。它**不是
//     安全边界**——`bash -c 'cd /; …'` 照样能离开工作目录。只有在没有 Bubblewrap
//     的平台上（macOS / Windows）才该用它，且要明白它挡的是「顺手写错」而不是恶意。
//
// 无论哪一档，下面三件都强制：**工作目录固定**（cwd 参数做词法 + 符号链接越界检查）、
// **硬超时 + 输出上限**、**命令白名单/黑名单**（按段取每条串联/管道命令的首个词比对）。
//
// ## 命令策略只是护栏
//
// 首个词检查挡不住 `bash -c '…'`、`eval`、解释器之类的刻意绕过。它拦的是模型
// 顺手发出的明显破坏性命令（`mount`、`shutdown`、`sudo`…）。**真正的隔离靠 bwrap**。
//
// ## 超时为什么要连进程组一起杀
//
// `bash -c` 自己会 fork 子进程（管道、后台任务）。只杀 bash 的话，`sleep 100 &`
// 这类子进程会留在超时之后继续跑。所以 Unix 下给子进程单独开一个进程组，超时对
// **整个组**发 SIGKILL（见 proc_unix.go）。bwrap 还额外带 `--die-with-parent`。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 默认与上限。**超时是硬约束**：模型给的 timeout_sec 会被夹进 [1, MaxTimeout]，
// 不设时用 DefaultTimeout。上限的存在是为了让一次调用最坏也只占这么久。
const (
	DefaultTimeout   = 30 * time.Second
	MaxTimeout       = 300 * time.Second
	DefaultStreamCap = 32 * 1024
)

// 沙盒模式。
const (
	// ModeBwrap 用 Bubblewrap 起真实命名空间沙盒（默认）。
	ModeBwrap = "bwrap"
	// ModeDirect 直接起 bash，只固定 cwd——**不是安全边界**，见文件头。
	ModeDirect = "direct"
)

// Options 建沙盒的配置。用结构体而不是一长串参数：可选项还会长。
type Options struct {
	// Root 沙盒根。所有命令的 cwd 都在它里面，bwrap 模式下也是唯一可写处。
	Root string
	// Allow 白名单。非空时进入白名单模式：每条命令的首个词都必须在里面。
	Allow []string
	// Deny 黑名单。始终生效，与 Allow 无关，且会与内置黑名单合并。
	Deny []string
	// Mode 见 ModeBwrap / ModeDirect。空 = ModeBwrap。
	Mode string
	// Bwrap 二进制路径。空 = 从 PATH 找 `bwrap`。仅 bwrap 模式用。
	Bwrap string
	// BwrapArgs 追加给 bwrap 的额外参数（插在 `--` 之前），高级用法。
	BwrapArgs []string
	// ShareNetwork 为 true 时保留宿主网络；默认 false = 起独立网络命名空间。
	ShareNetwork bool
}

// Sandbox 一次运行的沙盒配置。**启动时建一次，之后只读**。
type Sandbox struct {
	Root         string
	Shell        string
	Mode         string
	Bwrap        string
	BwrapArgs    []string
	ShareNetwork bool
	Allow        map[string]bool
	Deny         map[string]bool

	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	StreamCap      int

	rootReal string
	tmpDir   string
}

// RunRequest 一条待执行的命令。
type RunRequest struct {
	Command string
	// Cwd 沙盒根的**相对**子目录。空 = 根。绝对路径与 `..` 越界一律拒绝。
	Cwd string
	// Timeout 覆盖默认超时。<=0 用默认；超过 MaxTimeout 夹到上限。
	Timeout time.Duration
}

// Result 一条命令跑完的样子。**Run 返回 Result 不等于命令成功**——命令以非 0
// 退出、甚至被 bwrap 的启动问题挡下，都是正常结果，退出码与 stderr 在字段里。
type Result struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	TimedOut  bool
	Truncated bool
	Duration  time.Duration
}

// NewSandbox 建一个沙盒：把根转成绝对路径、建好目录、算好符号链接后的真实根。
//
// bwrap 模式下会在这里就把 Bubblewrap 找好——**找不到就拒绝启动**，而不是运行时
// 静默退化成不隔离的直接执行（那正是最坏的失败形态：看起来配好了，其实没有沙盒）。
func NewSandbox(opts Options) (*Sandbox, error) {
	trimmed := strings.TrimSpace(opts.Root)
	if trimmed == "" {
		return nil, errors.New("沙盒根不能为空（默认应是 <安装根>/sandbox）")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("解析沙盒根失败：%w", err)
	}
	// 沙盒根是文件系统根的话，「可写沙盒」就成了「可写整台机器」——bwrap 模式下
	// 那句 `--bind / /` 会盖掉只读根。直接拒，别让人以为自己在沙盒里。
	if abs == string(filepath.Separator) {
		return nil, errors.New("沙盒根不能是文件系统根 /")
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("创建沙盒根失败：%w", err)
	}
	rootReal, err := filepath.EvalSymlinks(abs)
	if err != nil {
		rootReal = abs
	}
	tmp := filepath.Join(abs, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("创建沙盒临时目录失败：%w", err)
	}

	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = ModeBwrap
	}
	if mode != ModeBwrap && mode != ModeDirect {
		return nil, fmt.Errorf("沙盒模式 %q 认不出，只有 %s 与 %s", opts.Mode, ModeBwrap, ModeDirect)
	}

	s := &Sandbox{
		Root:           abs,
		Shell:          "bash",
		Mode:           mode,
		BwrapArgs:      opts.BwrapArgs,
		ShareNetwork:   opts.ShareNetwork,
		Allow:          toSet(opts.Allow),
		Deny:           toSet(opts.Deny),
		DefaultTimeout: DefaultTimeout,
		MaxTimeout:     MaxTimeout,
		StreamCap:      DefaultStreamCap,
		rootReal:       rootReal,
		tmpDir:         tmp,
	}
	for name := range defaultDeny {
		s.Deny[name] = true
	}

	if mode == ModeBwrap {
		name := strings.TrimSpace(opts.Bwrap)
		if name == "" {
			name = "bwrap"
		}
		resolved, err := exec.LookPath(name)
		if err != nil {
			return nil, fmt.Errorf(
				"沙盒模式是 bwrap，但找不到 Bubblewrap（%s）。请安装 bubblewrap"+
					"（Debian/Ubuntu: apt install bubblewrap），或用 --bwrap 指定路径；"+
					"确要降级到纯 Go 的不隔离模式，设 BASH_SANDBOX_MODE=direct 并明白它挡不住 cd", name)
		}
		s.Bwrap = resolved
	}
	return s, nil
}

// defaultDeny 内置黑名单：这些命令**只可能**改到沙盒之外的东西（系统、硬件、
// 别的进程、别的用户）。沙盒内的正常干活用不到它们，所以默认拦掉。
//
// 它刻意**不含** `rm`、`dd`、`mv` —— 那些在沙盒内是正当操作，且 bwrap 模式下
// 写边界已经把它们关在沙盒里。
var defaultDeny = map[string]bool{
	"mkfs": true, "fdisk": true, "parted": true, "mkswap": true,
	"swapon": true, "swapoff": true,
	"mount": true, "umount": true, "pivot_root": true, "chroot": true,
	"nsenter": true, "unshare": true, "setpriv": true,
	"bwrap":    true,
	"shutdown": true, "reboot": true, "halt": true, "poweroff": true,
	"init": true, "systemctl": true, "service": true,
	"iptables": true, "nft": true, "insmod": true, "modprobe": true, "rmmod": true,
	"useradd": true, "userdel": true, "groupadd": true, "groupdel": true,
	"passwd": true, "visudo": true,
	"sudo": true, "su": true, "doas": true,
	"kill": true, "pkill": true, "killall": true,
}

// ResolveDir 把 cwd 参数解成一个可交给 exec 的目录。
//
// 两道检查：**词法**（`..` 直接拼出去就拒）与**符号链接**（沙盒里一个指向外面的
// 软链不能被当成合法子目录）。目录不存在就在沙盒内建出来。
func (s *Sandbox) ResolveDir(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("cwd 只能是沙盒内的相对路径，不能是绝对路径：%q", raw)
	}
	candidate := filepath.Join(s.Root, filepath.FromSlash(trimmed))
	if !within(s.Root, candidate) {
		return "", fmt.Errorf("cwd 越出了沙盒（%q）：只允许沙盒根下的相对子目录", raw)
	}
	resolved, err := resolveExisting(candidate)
	if err != nil {
		return "", err
	}
	if !within(s.rootReal, resolved) {
		return "", fmt.Errorf("cwd 经符号链接越出了沙盒（%q）：只允许沙盒根下的相对子目录", raw)
	}
	if err := os.MkdirAll(candidate, 0o755); err != nil {
		return "", fmt.Errorf("创建沙盒工作目录失败：%w", err)
	}
	return candidate, nil
}

// Check 按策略检查一条命令。返回 nil 表示放行。
//
// **逐段检查**：`ls | grep x` 里的 `ls` 与 `grep` 都要过；`a && b` 同理。
// 首个词的提取是启发式的，已知绕过见文件头。
func (s *Sandbox) Check(command string) error {
	if strings.TrimSpace(command) == "" {
		return errors.New("command 不能为空")
	}
	programs := commandPrograms(command)
	if len(programs) == 0 {
		return errors.New("无法从命令里识别出要执行的程序，已拒绝执行")
	}
	for _, p := range programs {
		if s.Deny[p] {
			return fmt.Errorf("命令 %q 在内置/配置的禁用列表里，已拒绝执行", p)
		}
	}
	if len(s.Allow) > 0 {
		for _, p := range programs {
			if !s.Allow[p] {
				return fmt.Errorf("命令 %q 不在允许列表里，已拒绝执行", p)
			}
		}
	}
	return nil
}

// Run 跑一条命令。**只对「没能跑起来」返 error**（策略拒、cwd 非法、启动失败）；
// 命令跑完了但退出码非 0、或超时被杀，都走 Result，由调用方决定怎么说给模型。
func (s *Sandbox) Run(ctx context.Context, req RunRequest) (Result, error) {
	if err := s.Check(req.Command); err != nil {
		return Result{}, err
	}
	dir, err := s.ResolveDir(req.Cwd)
	if err != nil {
		return Result{}, err
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = s.DefaultTimeout
	}
	if timeout > s.MaxTimeout {
		timeout = s.MaxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := s.invocation(dir, req.Command)
	cmd := exec.CommandContext(runCtx, name, args...)
	// bwrap 自己需要个宿主 cwd；direct 模式下就是命令真正的工作目录
	cmd.Dir = s.Root
	if s.Mode == ModeDirect {
		cmd.Dir = dir
	}
	cmd.Env = s.environ()
	configureProcess(cmd)
	// 超时时杀的是**整个进程组**，不只是外壳自己。
	cmd.Cancel = func() error { return killProcess(cmd) }
	cmd.WaitDelay = 5 * time.Second

	stdout := newCapWriter(s.StreamCap)
	stderr := newCapWriter(s.StreamCap)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("启动命令失败：%w", err)
	}
	waitErr := cmd.Wait()
	result := Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.truncated || stderr.truncated,
		Duration:  time.Since(started),
	}

	if runCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		return result, nil
	}
	if waitErr == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return Result{}, fmt.Errorf("命令执行失败：%w", waitErr)
}

// invocation 拼出真正要执行的命令（可执行文件 + 参数）。
func (s *Sandbox) invocation(dir, command string) (string, []string) {
	if s.Mode == ModeDirect {
		return s.Shell, []string{"-c", command}
	}
	return s.Bwrap, s.bwrapArgs(dir, command)
}

// bwrapArgs 拼 Bubblewrap 的参数。**顺序不能乱**：`--bind <root> <root>` 必须排在
// `--ro-bind / /` 与 `--tmpfs /tmp` **之后**，否则沙盒根会被随后挂上的只读根或
// tmpfs 盖住（沙盒根常落在 /tmp 下，先 bind 后 tmpfs 会让它消失、--chdir 直接失败）。
func (s *Sandbox) bwrapArgs(dir, command string) []string {
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-all",
	}
	if s.ShareNetwork {
		args = append(args, "--share-net")
	}
	// 只读全盘 → 一个可写的 /tmp → 新 proc/dev，最后才把沙盒根绑成可写
	args = append(args,
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
		"--bind", s.Root, s.Root,
	)
	args = append(args, s.BwrapArgs...)
	args = append(args,
		"--chdir", dir,
		"--setenv", "HOME", s.Root,
		"--setenv", "TMPDIR", s.tmpDir,
		"--",
		s.Shell, "-c", command,
	)
	return args
}

// environ 子进程环境。**继承宿主再收几样**，而不是白名单：
//
//   - HOME 指到沙盒根，`~`、`git config --global`、`~/.cache` 全落在沙盒里；
//   - TMPDIR 指到沙盒根下的 tmp/，临时文件不洒到宿主 /tmp；
//   - BASH_ENV / ENV / PWD 直接删掉——前两个会让 bash 启动时 source 任意文件
//     （绕过命令策略），PWD 交给 bash 自己按真实 cwd 重建。
func (s *Sandbox) environ() []string {
	drop := map[string]bool{"BASH_ENV": true, "ENV": true, "PWD": true, "HOME": true, "TMPDIR": true}
	base := os.Environ()
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if key, _, ok := strings.Cut(kv, "="); ok && drop[key] {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "HOME="+s.Root, "TMPDIR="+s.tmpDir)
	return out
}

// within 判断 path 是不是 root 之内（含 root 本身）。
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveExisting 解析路径的符号链接，**只解析已存在的那一段**，剩下的原样接回。
func resolveExisting(path string) (string, error) {
	current := path
	remaining := ""
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if remaining == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, remaining), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path, nil
		}
		remaining = filepath.Join(filepath.Base(current), remaining)
		current = parent
	}
}

// capWriter 一个**限量但照单全收**的写入器。
//
// ## 为什么超限了还要返回「写成功」
//
// 达到上限后如果返回短写（io.ErrShortWrite），子进程会因为管道出错而死——那会
// 把「输出太多」伪装成「命令失败」。所以它永远返回 len(p)，只是把超出的字节丢掉，
// 并记一个 truncated 标记。**必须继续把管道读干**，否则子进程照样会阻塞在写管道上。
type capWriter struct {
	buf       bytes.Buffer
	remaining int
	truncated bool
}

func newCapWriter(limit int) *capWriter {
	if limit <= 0 {
		limit = DefaultStreamCap
	}
	return &capWriter{remaining: limit}
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.remaining > 0 {
		take := p
		if len(take) > w.remaining {
			take = take[:w.remaining]
		}
		w.buf.Write(take)
		w.remaining -= len(take)
		if len(take) < len(p) {
			w.truncated = true
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

func (w *capWriter) String() string { return w.buf.String() }

// toSet 把名字列表转成小写集合，忽略空白项。
func toSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		if trimmed := strings.ToLower(strings.TrimSpace(name)); trimmed != "" {
			out[trimmed] = true
		}
	}
	return out
}

// wrappers 以别的程序为参数的「前缀命令」。它们的**自身名字**也会进策略比对，
// 之后的参数按 flags / 赋值跳过；个别需要吃掉一个位置参数的记在 wrapperValues。
var wrappers = map[string]bool{
	"env": true, "nohup": true, "command": true, "exec": true,
	"doas": true, "sudo": true, "setsid": true, "time": true,
	"timeout": true, "nice": true, "ionice": true, "stdbuf": true, "xargs": true,
}

// wrapperValues 前缀命令在 flags 之后、真正的命令之前还要吃掉的**位置参数**个数。
var wrapperValues = map[string]int{"timeout": 1, "nice": 1, "ionice": 2, "stdbuf": 1}

// commandPrograms 从一条命令里抽出参与策略比对的程序名。
//
// 做法是启发式的：按 shell 控制符切成段，每段跳过前缀命令与 `VAR=值` 赋值后，
// 取**首个词**的 basename。前缀命令的名字本身也收进来（`sudo …` 因此能被
// 内置黑名单拦下）。已知绕过见文件头——它是护栏不是墙。
func commandPrograms(command string) []string {
	var programs []string
	for _, segment := range splitCommands(command) {
		fields := strings.Fields(segment)
		i := 0
		for i < len(fields) && isAssignment(fields[i]) {
			i++
		}
		for i < len(fields) && wrappers[base(fields[i])] {
			name := base(fields[i])
			programs = append(programs, name)
			i++
			for i < len(fields) && (strings.HasPrefix(fields[i], "-") || isAssignment(fields[i])) {
				i++
			}
			for n := 0; n < wrapperValues[name] && i < len(fields) && !strings.HasPrefix(fields[i], "-"); n++ {
				i++
			}
		}
		if i < len(fields) {
			if program := base(fields[i]); program != "" {
				programs = append(programs, program)
			}
		}
	}
	return programs
}

// splitCommands 按 shell 控制符切分。`&&`/`||` 会被 `&`/`|` 各自切开，无妨——
// 切碎了只会让某段为空，不会漏掉一个程序。`$(`、反引号里的命令也因此进入比对。
func splitCommands(command string) []string {
	var segments []string
	var current strings.Builder
	flush := func() {
		segments = append(segments, current.String())
		current.Reset()
	}
	for _, r := range command {
		switch r {
		case '\n', '\r', ';', '|', '&', '(', ')', '`', '{', '}':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return segments
}

// base 取一个词的 basename 并去掉包裹的引号。
func base(token string) string {
	token = strings.TrimSpace(strings.Trim(token, `"'`))
	if token == "" {
		return ""
	}
	if i := strings.LastIndexAny(token, `/\`); i >= 0 {
		token = token[i+1:]
	}
	return token
}

// isAssignment 判断一个词是不是 `VAR=值` 形式的环境变量赋值。
func isAssignment(token string) bool {
	eq := strings.IndexByte(token, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range token[:eq] {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
