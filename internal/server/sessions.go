package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/frpsclient"
)

const (
	// 在线会话页默认每页条数与上限
	sessionsDefaultPageSize = 50
	sessionsMaxPageSize     = 200
)

// sessionsPage 是在线会话页视图（5 秒自动刷新；支持下线操作）。
type sessionsPage struct {
	pageBase
	Sessions   []frpsclient.RadiusSessionItem
	Total      int
	Page       int
	PageSize   int
	TotalPages int
	Keyword    string
	Pages      []pageLink
	Err        string
	FlashOK    string
	FlashErr   string
	// 页脚提示：当前超时阈值、认证源与超时是否向上游补发 Stop
	Timeout       int
	AuthSource    string
	StopOnTimeout bool
}

// sessionsQuery 解析在线会话页的分页与关键字参数（页面与轮询 JSON 共用）。
func sessionsQuery(c *gin.Context) (page, pageSize int, keyword string) {
	page = atoiDefault(c.Query("page"), 1)
	pageSize = atoiDefault(c.Query("page_size"), sessionsDefaultPageSize)
	if pageSize > sessionsMaxPageSize {
		pageSize = sessionsMaxPageSize
	}
	keyword = strings.TrimSpace(c.Query("keyword"))
	return page, pageSize, keyword
}

// handleSessionsPage 渲染在线会话页（主菜单「在线会话」）。
func (a *App) handleSessionsPage(c *gin.Context) {
	u, _ := auth.CurrentUser(c)
	page, pageSize, keyword := sessionsQuery(c)
	pg := sessionsPage{
		pageBase: pageBase{ActiveMenu: "sessions", CurrentUser: u, Config: a.Cfg},
		Page:     page,
		PageSize: pageSize,
		Keyword:  keyword,
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

	res, err := a.Frps.ListRadiusSessions(c.Request.Context(), page, pageSize, keyword)
	if err != nil {
		a.Log.Error("list radius sessions failed", "err", err)
		pg.Err = "获取活跃记账会话失败：" + err.Error()
	} else if res != nil {
		pg.Sessions = res.Items
		pg.Total = res.Total
	}

	pg.TotalPages = (pg.Total + pageSize - 1) / pageSize
	if pg.TotalPages < 1 {
		pg.TotalPages = 1
	}
	pg.Pages = buildPager(page, pg.TotalPages, func(n int) string {
		return sessionsPageURL(n, pageSize, keyword)
	})

	if rerr := a.Render.Render(c.Writer, "pages/sessions/index", pg); rerr != nil {
		a.Log.Error("render sessions page failed", "err", rerr)
	}
}

// handleSessionsData 供在线会话页轮询（只读 JSON，支持分页与关键字）。
func (a *App) handleSessionsData(c *gin.Context) {
	page, pageSize, keyword := sessionsQuery(c)
	res, err := a.Frps.ListRadiusSessions(c.Request.Context(), page, pageSize, keyword)
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

// sessionsPageURL 构造在线会话分页链接（保留每页条数与关键字）。
func sessionsPageURL(n, pageSize int, keyword string) string {
	q := url.Values{}
	if keyword != "" {
		q.Set("keyword", keyword)
	}
	if pageSize != sessionsDefaultPageSize {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	q.Set("page", strconv.Itoa(n))
	return "/sessions?" + q.Encode()
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
	// 保留当前分页与关键字，下线后回到同一视图
	backQuery := url.Values{}
	if kw := strings.TrimSpace(c.PostForm("keyword")); kw != "" {
		backQuery.Set("keyword", kw)
	}
	if p := atoiDefault(c.PostForm("page"), 1); p > 1 {
		backQuery.Set("page", strconv.Itoa(p))
	}
	if size := atoiDefault(c.PostForm("page_size"), sessionsDefaultPageSize); size != sessionsDefaultPageSize {
		backQuery.Set("page_size", strconv.Itoa(size))
	}
	if len(backQuery) > 0 {
		back += "?" + backQuery.Encode()
	}
	redirectWith := func(key, msg string) {
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		c.Redirect(http.StatusFound, back+sep+key+"="+url.QueryEscape(msg))
	}
	okMsg := func(msg string) { redirectWith("ok", msg) }
	errMsg := func(msg string) { redirectWith("err", msg) }

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
