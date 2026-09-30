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
	qrPollGap     = 2 * time.Second
)

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
// onStatus 每次状态变化调一次（给控制面转发给 CLI）。ctx 取消即中止——
// **CLI 被 Ctrl-C 后服务不该继续替一个没人看的二维码轮询**。
func PollQRCodeStatus(ctx context.Context, baseURL string, httpClient *http.Client, token string,
	onStatus func(string)) (Credentials, error) {

	if token == "" {
		return Credentials{}, fmt.Errorf("qrcode 令牌为空：无法轮询扫码状态")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	c := NewClient(WeixinAccount{BaseURL: baseURL}, httpClient)
	for {
		if ctx.Err() != nil {
			return Credentials{}, fmt.Errorf("登录已取消")
		}

		credentials, done, err := checkQRCodeStatus(ctx, c, token)
		if done {
			return credentials, err
		}
		if err != nil {
			// 中断导致的失败不是错误；其它错误是网络/协议问题，**重试**
			if ctx.Err() != nil {
				return Credentials{}, fmt.Errorf("登录已取消")
			}
			if !isTimeout(err) {
				return Credentials{}, fmt.Errorf("查询扫码状态失败：%w", err)
			}
		}

		if onStatus != nil {
			onStatus(QRStatusWait)
		}
		if !sleepCtxOK(ctx, qrPollGap) {
			return Credentials{}, fmt.Errorf("登录已取消")
		}
	}
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
func checkQRCodeStatus(ctx context.Context, c *client, token string) (Credentials, bool, error) {
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
	if err := os.WriteFile(envPath, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("写 %s 失败：%w", envPath, err)
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
