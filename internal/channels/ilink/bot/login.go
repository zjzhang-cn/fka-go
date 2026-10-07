package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// iLink 二维码登录。
//
// 流程：
//  1. 获取二维码（令牌 + 要编码进码面的内容）
//  2. 轮询扫码状态（wait → scaned → confirmed）
//  3. 保存登录凭证
//
// **码面内容与轮询令牌是两样东西**——见 QRPayload，那里记着踩过的坑。

// DefaultBaseURL iLink 服务基址。
//
// **它由调用方传进来而不是写死**：登录要打的是「还没登录的那个服务」，
// 与账号自己的 BaseURL 是两回事（后者是登录后才拿到的）。写成参数还顺带让
// 测试能指向本地服务器——写死的话测试会真的打到微信去。
const DefaultBaseURL = "https://ilinkai.weixin.qq.com"

// BotType 机器人类型。协议里是 query 参数 `bot_type`。
const BotType = 3

// 扫码状态机的四个状态。
const (
	QRStatusWait      = "wait"
	QRStatusScanned   = "scaned"
	QRStatusConfirmed = "confirmed"
	QRStatusExpired   = "expired"
)

// 二维码轮询参数。
const (
	qrPollTimeout = 40 * time.Second
	// qrPollMaxFailures 连续失败多少次就放弃这一个二维码。
	//
	// **上限和重试缺一不可**：重试是「网络抖一下」的解法，可服务端真挂了的时候，
	// 没有上限的重试会让人对着一个**早就失效的二维码**一直转圈——界面上什么都不说，
	// 而正确的做法是告诉他「查不了，重新扫一个」。反过来也不能只试一次：抖动是常态，
	// 一次就放弃等于把登录做成了碰运气。
	qrPollMaxFailures = 5
)

// qrPollGap 两轮之间睡多久。
//
// **是 var 不是 const**：用例要把它调短——「失败几次才放弃」这类用例得真的
// 走完整个轮询循环，按 2 秒算一条用例就要十几秒。改它只影响轮询节奏，
// 不影响任何协议语义。
var qrPollGap = 2 * time.Second

// QRCode 一个二维码。**令牌与码面内容是两样东西。**
type QRCode struct {
	// QRCode 轮询用的令牌
	QRCode string
	// ImgContent 该编码进**码面**的内容
	ImgContent string
}

// Credentials 登录凭证。
type Credentials struct {
	BotToken    string
	BaseURL     string
	ILinkBotID  string
	ILinkUserID string
}

// GetQRCode 获取二维码。
//
// 请求形状按官方协议：**POST**，body 带 `local_token_list`（本机已持有的
// bot token，最多十条；服务端据此判断扫码的微信号是否已绑定过 bot）。
// 这里给空列表——协议允许为空，且本项目的多账号各自独立扫码，不依赖它。
func GetQRCode(ctx context.Context, baseURL string, httpClient *http.Client) (QRCode, error) {
	var result QRCode
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	c := NewClient(WeixinAccount{BaseURL: baseURL}, httpClient)
	data, err := c.postJSON(ctx,
		fmt.Sprintf("/ilink/bot/get_bot_qrcode?bot_type=%d", BotType),
		struct {
			LocalTokenList []string `json:"local_token_list"`
		}{LocalTokenList: []string{}},
		30*time.Second)
	if err != nil {
		return result, fmt.Errorf("获取二维码失败：%w", err)
	}

	var payload struct {
		QRCode        string `json:"qrcode"`
		QRCodeImgCont string `json:"qrcode_img_content"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return result, fmt.Errorf("解析二维码响应失败：%w", err)
	}

	result.QRCode = payload.QRCode
	result.ImgContent = payload.QRCodeImgCont
	return result, nil
}

// QRPayload 该编码进二维码**码面**的内容。
//
// ## 这是踩过的一个坑
//
// 原先编码的是 `qrcode` 令牌本身（一串 32 位十六进制），扫出来是一段纯文本，
// 微信不认——**码面看着完全正常，扫了却没反应**。正确的内容是服务端在同一个
// 响应里给的 `qrcode_img_content`：
//
//	https://liteapp.weixin.qq.com/q/7GiQu1?qrcode=<令牌>&bot_type=3
//
// 顺带钉住一条不变量：**URL 里的 `qrcode` 必须与轮询用的是同一个值。**
// 不一致时用户扫开的是另一个没人在轮询的会话——表现为「扫了、也确认了，
// 然后一直等下去」，是最难查的一类故障。内容由服务端给之后，这条得显式校验。
func QRPayload(code QRCode) (string, error) {
	content := strings.TrimSpace(code.ImgContent)
	if content == "" {
		return "", fmt.Errorf("服务端没有返回二维码内容（qrcode_img_content 为空），生成不出可扫的码。" +
			"这多半是服务端行为变了，先别扫码")
	}

	inURL := qrcodeParamOf(content)
	// 内容不一定是 URL（取不到参数就跳过校验），但取到了就必须一致
	if inURL != "" && code.QRCode != "" && inURL != code.QRCode {
		return "", fmt.Errorf("二维码内容里的 qrcode（%s）与轮询用的（%s）不一致，扫了会等不到确认。"+
			"多半是服务端行为变了，先别扫码", inURL, code.QRCode)
	}
	return content, nil
}

// qrcodeParamOf 从链接里取出 `qrcode` 参数。内容不是 URL 时返回空串。
func qrcodeParamOf(content string) string {
	parsed, err := url.Parse(content)
	if err != nil {
		return ""
	}
	return parsed.Query().Get("qrcode")
}

// PollQRCodeStatus 轮询扫码状态直到确认或过期。
//
// onStatus **每次状态变化**调一次（给控制面转发给 CLI）。ctx 取消即中止——
// **CLI 被 Ctrl-C 后服务不该继续替一个没人看的二维码轮询**。
//
// ## 「变化」是这里的关键字
//
// 以前它在每一轮无条件 `onStatus(QRStatusWait)`，而轮询间隔是 2 秒——于是 CLI 每 2 秒
// 重印一次「等待扫码…」，而 `QRStatusScanned`（"scaned"）**从来没被上报过**：
// CLI 里那句「扫到了，在手机上确认…」是一段死代码，用户扫码后界面上毫无反应，
// 直到确认成功。
//
// 现在由 `checkQRCodeStatus` 报它**这一轮看到的状态**，这里只做去重。
//
// ## 查状态失败怎么办：**重试，但有上限**
//
// 网络/协议问题（连不上、HTTP 5xx、应答不是 JSON、等太久）一律重试，连续
// `qrPollMaxFailures` 次才放弃——见 `qrPollAfterFailure`。这里以前的判据是
// 反的：只有**超时**才继续轮询，快速失败反而立刻终止整次登录。
func PollQRCodeStatus(ctx context.Context, baseURL string, httpClient *http.Client, token string,
	onStatus func(string)) (Credentials, error) {

	if token == "" {
		return Credentials{}, fmt.Errorf("qrcode 令牌为空：无法轮询扫码状态")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	reporter := newStatusReporter(onStatus)

	c := NewClient(WeixinAccount{BaseURL: baseURL}, httpClient)
	consecutive := 0
	for {
		if ctx.Err() != nil {
			return Credentials{}, fmt.Errorf("登录已取消")
		}

		credentials, done, err := checkQRCodeStatus(ctx, c, token, reporter.report)
		if done {
			return credentials, err
		}
		if err != nil {
			consecutive++
			retry, failure := qrPollAfterFailure(ctx, err, consecutive)
			if !retry {
				return Credentials{}, failure
			}
		} else {
			// 查成功了就把连续失败的计数清零：中途抖两下不该攒成一次放弃
			consecutive = 0
		}

		if !sleepCtxOK(ctx, qrPollGap) {
			return Credentials{}, fmt.Errorf("登录已取消")
		}
	}
}

// qrPollAfterFailure 一次「查扫码状态」失败之后该怎么办。
//
// 返回 **(要不要重试, 放弃时交给用户的错误)**。纯逻辑，不碰网络也不睡觉——
// 真实的 2 秒轮询间隔没法在用例里等，所以判据必须能单独拎出来测。
//
// ## 为什么判据与以前正好相反
//
// 旧实现是：
//
//	// 中断导致的失败不是错误；其它错误是网络/协议问题，**重试**
//	if !isTimeout(err) {
//		return Credentials{}, fmt.Errorf("查询扫码状态失败：%w", err)
//	}
//
// 注释说「其它错误要重试」，代码却在**不是超时**的时候直接放弃——于是：
//
//   - 服务端**挂起**（40s 超时）→ 重试；
//   - 连接被拒 / 连接被重置 / HTTP 5xx / 应答不是 JSON（**快速失败**）→
//     立刻终止整次登录，而用户扫的那个码已经作废。
//
// 挂起会重试、连不上反而不重试，这个方向对网络抖动来说是最糟的组合——
// 而扫码窗口只有十分钟，任何一次抖动都可能让已扫的码白扫。
//
// 取消单独一条：**Ctrl-C 之后不该继续替一个没人看的二维码轮询**，
// 那时候报一句「登录已取消」比报原始错误有用。
func qrPollAfterFailure(ctx context.Context, err error, consecutive int) (bool, error) {
	if ctx.Err() != nil {
		return false, fmt.Errorf("登录已取消")
	}
	if consecutive >= qrPollMaxFailures {
		return false, fmt.Errorf("连续 %d 次查询扫码状态都失败了：%w。"+
			"多半是网络或服务端的问题，二维码可能也已经过期，请重新获取一个",
			consecutive, err)
	}
	return true, nil
}

// statusReporter 只上报**变化**的状态。
//
// 轮询每 2 秒问一次「现在什么状态」，而服务端的答案绝大多数时候是同一个 `wait`。
// **重复报同一句话既没用又吵**：终端会每 2 秒重印一次「等待扫码…」。
//
// 摘成一个纯逻辑的小类型，是为了让它能被直接测——真实的 2 秒轮询间隔没法在用例里等。
type statusReporter struct {
	onStatus func(status string)
	last     string
}

func newStatusReporter(onStatus func(string)) *statusReporter {
	return &statusReporter{onStatus: onStatus}
}

// report 状态与上次不同才交出去。空状态不算状态（服务端没给 `status` 时）。
func (r *statusReporter) report(status string) {
	if r.onStatus == nil || status == "" || status == r.last {
		return
	}
	r.last = status
	r.onStatus(status)
}

// sleepCtxOK 睡一会儿，**响应 ctx 取消**。返回 false 表示被取消了。
func sleepCtxOK(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// checkQRCodeStatus 查一次状态。
//
// 第二个返回值 true 表示**轮询该结束了**（已确认或已过期）。
//
// `report` 报这一轮**看到的状态**（`wait` / `scaned` / 服务端将来新加的别的中间态）：
// 终态（confirmed / expired）不走它——它们由返回值表达，调用方那边有更好的话可以说。
func checkQRCodeStatus(ctx context.Context, c *client, token string,
	report func(string)) (Credentials, bool, error) {
	endpoint := c.baseURL + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(token)

	if timeout := qrPollTimeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Credentials{}, false, err
	}
	// 这两个头是登录接口专用的，与 getupdates 那套鉴权不同
	request.Header.Set("iLink-App-ClientVersion", "1")
	request.Header.Set("SKRouteTag", "1001")

	response, err := c.http.Do(request)
	if err != nil {
		return Credentials{}, false, err
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return Credentials{}, false, err
	}

	// **看状态码，别只管反序列化**。这条路径以前直接 `json.Unmarshal` 应答体，
	// 于是网关回的 500 HTML 会变成一句「解析扫码状态失败」——真正的故障
	// （服务端挂了）被说成了「格式不对」，排查方向整个带偏。
	//
	// 同一次登录里 `GetQRCode` 走的是 `postJSON` → `do()`，那边 `>= 400` 早就拦了：
	// 同一条链上不该有两套 HTTP 语义。这里的错误归「可重试」，由
	// `qrPollAfterFailure` 决定试几次。
	if response.StatusCode >= 400 {
		return Credentials{}, false, fmt.Errorf("查询扫码状态失败：HTTP %d %s",
			response.StatusCode, snippet(body))
	}

	var payload struct {
		Status      string `json:"status"`
		BotToken    string `json:"bot_token"`
		ILinkBotID  string `json:"ilink_bot_id"`
		ILinkUserID string `json:"ilink_user_id"`
		BaseURL     string `json:"baseurl"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Credentials{}, false, fmt.Errorf("解析扫码状态失败：%w", err)
	}

	switch payload.Status {
	case QRStatusConfirmed:
		if payload.BotToken == "" {
			// 声称已确认却不给 token —— 那不是「登录成功」，别当成功
			return Credentials{}, true, fmt.Errorf("服务端说已确认，但没给 bot_token")
		}
		return Credentials{
			BotToken:    payload.BotToken,
			BaseURL:     payload.BaseURL,
			ILinkBotID:  payload.ILinkBotID,
			ILinkUserID: payload.ILinkUserID,
		}, true, nil
	case QRStatusExpired:
		return Credentials{}, true, fmt.Errorf("二维码已过期，请重新获取")
	default:
		// 中间态（`wait` / **`scaned`** / 将来新加的）如实上报，去重交给调用方。
		// `scaned` 这条以前没人报，于是 CLI 里「扫到了，在手机上确认…」是死代码
		// ——用户扫码之后界面上毫无反应，直到确认成功
		if report != nil {
			report(payload.Status)
		}
		return Credentials{}, false, nil
	}
}

// ── 凭证落盘 ────────────────────────────────────────────

// 写哪个 `.env` 由调用方给：**安装根下的那一份**（`config.EnvPath()`），与
// `config.LoadEnv` 读的是同一个。
//
// 这里以前自己实现了一遍安装根解析（`FKA_HOME > 可执行文件目录 > "."`）。同一份
// 安装里两处各解析一次，迟早在某个启动方式下指到不同的盘上——而「登录看着成功、
// 重启后账号消失」正是那个坑的表现。所以路径**没有默认值**，调用方必须显式传。

// SaveCredentials 把登录凭证写进 `.env` 的对应账号块。
//
// ## 为什么必须是「临时文件 + rename」，不能 `os.WriteFile`
//
// `os.WriteFile` = `O_TRUNC` + `write`——**先截断，再写**。所以任何一次写失败
// 或写到一半被杀，留在原地的都是**空文件或半截文件**，而这份文件里是全部账号
// 的 bot token、`LLM_API_KEY`、`LOG_LEVEL`……一整份配置，没有备份，事后也看
// 不出来发生过（登录界面只报「写失败」）。
//
// 磁盘满（`ENOSPC`）时最坏：`write` 报错，函数如实返错，**但文件已经被截断了**。
// 与同一个包里 `cursor.go` 的 `persistLocked` 是同一条顾虑——那边为更不值钱的
// 游标专门做了 temp+rename，这条路上却一直是直接覆盖。
//
// 顺带解决权限：`os.WriteFile` 的 perm 参数**只在 `O_CREATE` 时生效**，于是按
// `.env.example`（0644）拷出来的那份 `.env` 登录成功后仍然是 0644。改名替换
// 之后，文件的权限来自那个 0600 的临时文件，**每次写完都回到 0600**。
func SaveCredentials(envPath string, accountIndex int, credentials Credentials) error {
	original, err := os.ReadFile(envPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读 %s 失败：%w", envPath, err)
	}

	block := AccountBlock(accountIndex, credentials)
	updated := ReplaceAccountBlock(string(original), accountIndex, block)

	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		return fmt.Errorf("建 .env 目录失败：%w", err)
	}
	// **0600**：里面有 bot token
	if err := atomicWriteFile(envPath, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("写 %s 失败：%w", envPath, err)
	}
	return nil
}

// atomicWriteFile 用「临时文件 + fsync + rename」替换 path 的内容。
//
// **临时文件名带随机后缀**（`os.CreateTemp`）而不是固定 `.tmp`：两个 `fka login`
// 并发时固定名会互相 `O_TRUNC`，又是一次截断。rename 在同一目录内是原子的，
// 所以读者要么看到旧内容、要么看到新内容，永远看不到中间态。
//
// 失败路径**一律删掉临时文件并原样返回**：目标文件一个字节都不动。
// 「写失败但原文件已经没了」比「写失败」坏得多——前者是数据没了，后者只是这次
// 登录没成。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("建临时文件失败：%w", err)
	}
	temp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(temp)
	}

	// `CreateTemp` 已经是 0600，这里显式再设一次：perm 是调用方声明的意图，
	// 不该依赖某个 stdlib 的默认值
	if err := f.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("设置临时文件权限失败：%w", err)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("写临时文件失败：%w", err)
	}
	// **先落盘再改名**：不 Sync 的话 rename 可能先于数据到达磁盘，断电后拿到
	// 一个空的目标文件——那正是这次改动要消灭的东西
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("落盘失败：%w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("关闭临时文件失败：%w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("替换目标文件失败：%w", err)
	}
	return nil
}

// AccountBlock 一个账号在 `.env` 里的那几行。
func AccountBlock(accountIndex int, credentials Credentials) []string {
	return []string{
		fmt.Sprintf("# 账号 %d", accountIndex),
		fmt.Sprintf("ILINK_ACCOUNT_%d_ID=%s", accountIndex, accountSlotID(accountIndex)),
		fmt.Sprintf("ILINK_ACCOUNT_%d_BOT_TOKEN=%s", accountIndex, credentials.BotToken),
		fmt.Sprintf("ILINK_ACCOUNT_%d_BASE_URL=%s", accountIndex, credentials.BaseURL),
		fmt.Sprintf("ILINK_ACCOUNT_%d_BOT_ID=%s", accountIndex, credentials.ILinkBotID),
		fmt.Sprintf("ILINK_ACCOUNT_%d_USER_ID=%s", accountIndex, credentials.ILinkUserID),
	}
}

// accountSlotID 账号槽位标识，如 `account_002`。
//
// **补零到三位**：账号号会进日志、进会话历史文件名，补零让它们在同一列上，
// 而 `account_2` 与 `account_002` 混用会让排查时以为有两个账号。
func accountSlotID(accountIndex int) string {
	return "account_" + fmt.Sprintf("%03d", accountIndex)
}

// ReplaceAccountBlock 就地把该账号的块替换为新配置；找不到已有块则在文件末尾追加。
//
// ## 为什么按行扫描定位，而不是用正则匹配块边界
//
// 起因是一个真实缺陷：`.env.example` 里账号 2 的标签是 `# 账号2（可选）`——
// **账号号后面还有别的字**。原先的正则要求账号号后紧跟换行，于是匹配失败，
// 新配置被**追加到文件末尾**而不是就地替换，导致 `ILINK_ACCOUNT_2_ID` 出现两次。
// dotenv 通常后者覆盖前者，所以表面能跑；但一旦文件顺序变化，加载器就会读到
// 旧的占位值，表现为「账号莫名其妙登不上」。
//
// 规则：块从含 `账号 N` 的注释行开始，一直延伸到最后一个该账号的
// `ILINK_ACCOUNT_N_*` 行，注释行与配置行都算在内。
func ReplaceAccountBlock(content string, accountIndex int, block []string) string {
	lines := []string{}
	if content != "" {
		lines = strings.Split(content, "\n")
	}

	start := -1
	for i, line := range lines {
		if isAccountLabel(line, accountIndex) {
			start = i
			break
		}
	}

	if start < 0 {
		base := strings.TrimRight(content, "\n")
		if base != "" {
			base += "\n\n"
		}
		return base + strings.Join(block, "\n") + "\n"
	}

	// 向后吞掉该账号的所有配置行——包括主配置与 .env.example 里成组的注释行。
	// 用循环而非搜索定位末行，正是为了容纳中间可能夹着的注释。
	end := start + 1
	for end < len(lines) && isAccountSetting(lines[end], accountIndex) {
		end++
	}

	out := make([]string, 0, len(lines)-end+start+len(block))
	out = append(out, lines[:start]...)
	out = append(out, block...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

// isAccountLabel 判断某行是否为该账号的标签注释行，如 `# 账号 2` / `# 账号2（可选）`。
//
// **「账号」与数字之间允许有空格**——两种写法都在真实文件里出现过，
// 只认一种的话另一种会被当成「块不存在」而追加到文件末尾。
func isAccountLabel(line string, accountIndex int) bool {
	if !strings.HasPrefix(strings.TrimSpace(line), "#") {
		return false
	}
	return containsAccountNumber(line, accountIndex)
}

// containsAccountNumber 行里有没有「账号 [可选空格] N」，且 N 后面不能再跟数字。
//
// 最后那个「不能再跟数字」很重要：`# 账号2` 不该被认成账号 12 的标签，
// 而账号 12 的真实块在文件里通常就排在它后面。
func containsAccountNumber(line string, accountIndex int) bool {
	target := strconv.Itoa(accountIndex)
	rest := line
	for {
		at := strings.Index(rest, "账号")
		if at < 0 {
			return false
		}
		rest = rest[at+len("账号"):]

		rest = strings.TrimLeft(rest, " \t")
		if !strings.HasPrefix(rest, target) {
			continue
		}
		after := rest[len(target):]
		if after == "" || after[0] < '0' || after[0] > '9' {
			return true
		}
	}
}

// isAccountSetting 判断某行是否为该账号的配置项（含被注释掉的占位行）。
func isAccountSetting(line string, accountIndex int) bool {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
	prefix := "ILINK_ACCOUNT_" + strconv.Itoa(accountIndex) + "_"
	if !strings.HasPrefix(trimmed, prefix) {
		return false
	}
	// 得是 `KEY=VALUE` 形状，否则会把下一段的注释也吞进来
	rest := trimmed[len(prefix):]
	equals := strings.IndexByte(rest, '=')
	if equals <= 0 {
		return false
	}
	for _, r := range rest[:equals] {
		if r != '_' && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
