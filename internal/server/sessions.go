package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/frpsclient"
)

// 在线会话页首屏/轮询最多展示条数
const sessionsPageSize = 50

// sessionsPage 是在线会话页视图（5 秒自动刷新；支持下线操作）。
type sessionsPage struct {
	pageBase
	Sessions []frpsclient.RadiusSessionItem
	Total    int
	Err      string
	FlashOK  string
	FlashErr string
	// 页脚提示：当前超时阈值、认证源与超时是否向上游补发 Stop
	Timeout       int
	AuthSource    string
	StopOnTimeout bool
}

// handleSessionsPage 渲染在线会话页（主菜单「在线会话」）。
func (a *App) handleSessionsPage(c *gin.Context) {
	u, _ := auth.CurrentUser(c)
	pg := sessionsPage{
		pageBase: pageBase{ActiveMenu: "sessions", CurrentUser: u, Config: a.Cfg},
		Timeout:  radDefaultSessionTimeout,
		FlashOK:  c.Query("ok"),
		FlashErr: c.Query("err"),
	}

	if cfg, err := a.Frps.GetRadiusConfig(c.Request.Context()); err == nil && cfg != nil {
		if cfg.AcctSessionTimeout > 0 {
			pg.Timeout = cfg.AcctSessionTimeout
		}
		pg.AuthSource = cfg.AuthSource
		if pg.AuthSource == "" {
			pg.AuthSource = "local"
		}
		pg.StopOnTimeout = cfg.UpstreamStopOnTimeout
	}

	res, err := a.Frps.ListRadiusSessions(c.Request.Context(), 1, sessionsPageSize, "")
	if err != nil {
		a.Log.Error("list radius sessions failed", "err", err)
		pg.Err = "获取活跃记账会话失败：" + err.Error()
	} else if res != nil {
		pg.Sessions = res.Items
		pg.Total = res.Total
	}

	if rerr := a.Render.Render(c.Writer, "pages/sessions/index", pg); rerr != nil {
		a.Log.Error("render sessions page failed", "err", rerr)
	}
}

// handleSessionsData 供在线会话页轮询（只读 JSON）。
func (a *App) handleSessionsData(c *gin.Context) {
	res, err := a.Frps.ListRadiusSessions(c.Request.Context(), 1, sessionsPageSize, "")
	if err != nil {
		a.Log.Error("list radius sessions failed", "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if res == nil {
		res = &frpsclient.RadiusSessionListResult{}
	}
	c.JSON(http.StatusOK, res)
}

// handleSessionDrop 强制下线在线会话（POST /sessions/drop）。
// 链路：RADIUS 会话的 NAS-Identifier 即 device_id → GetDevice 取 frp SessionID →
// 经隧道 {session_id}.frpclient.local 调设备 ubus webauthd.session/drop_user
// （按 acct_session_id 精确匹配，仅下线在线 AUTHED/FREE 用户，见 webauthd UBUS_API 文档 §3）。
// drop_user 为异步接口：受理成功后设备侧 RADIUS Stop / 会话销毁在随后的关闭流程完成
// （RADIUS 在线时最长约 5 秒兜底），列表由 5 秒轮询自动刷新反映。
func (a *App) handleSessionDrop(c *gin.Context) {
	nas := strings.TrimSpace(c.PostForm("nas"))          // NAS-Identifier = device_id
	acctSID := strings.TrimSpace(c.PostForm("acct_sid")) // Acct-Session-Id
	back := "/sessions"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+url.QueryEscape(msg)) }

	if nas == "" || acctSID == "" {
		errMsg("缺少必要参数（nas / acct_sid）")
		return
	}
	dev, err := a.Frps.GetDevice(c.Request.Context(), nas)
	if err != nil {
		errMsg(fmt.Sprintf("设备 %s 未找到：%s", nas, err.Error()))
		return
	}
	if dev.SessionID == "" || dev.Status != "online" {
		errMsg(fmt.Sprintf("设备 %s 离线，无法下发下线指令", nas))
		return
	}
	var out struct {
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		Dropped int    `json:"dropped"`
	}
	if err := a.Ubus.CallWriteInto(c.Request.Context(), dev.SessionID,
		"webauthd.session", "drop_user", &out,
		map[string]any{"session_id": acctSID}); err != nil {
		// ubus 状态码 4 = NOT_FOUND：设备上已无匹配的在线用户（可能刚刚自行断开）
		if strings.Contains(err.Error(), "code 4") {
			errMsg("设备上未找到该在线用户（可能已断开），列表稍候自动刷新")
			return
		}
		errMsg("下线指令下发失败: " + err.Error())
		return
	}
	if out.Code != 0 {
		errMsg(fmt.Sprintf("设备拒绝下线请求：%s（code %d）", out.Msg, out.Code))
		return
	}
	okMsg(fmt.Sprintf("已下发下线指令（用户会话 %s），设备将在数秒内断开该用户", acctSID))
}
