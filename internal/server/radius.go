package server

import (
	"crypto/rand"
	"math/big"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/frpsclient"
)

// radDefaults RADIUS 配置展示/提交缺省值（与 frps_helper radius 包默认值一致）
const (
	radDefaultAuthPort         = 8812
	radDefaultAcctPort         = 8813
	radDefaultAuthTimeout      = 10
	radDefaultAcctTimeout      = 10
	radDefaultSessionTimeout   = 600
	radDefaultCleanupInterval  = 60
	radDefaultUpstreamAuthPort = 1812
	radDefaultUpstreamAcctPort = 1813
	radDefaultAuthSource       = "local"
	radDefaultLDAPPort         = 389
	radDefaultLDAPUserFilter   = "(uid=%s)"
	radDefaultLDAPTimeout      = 5
	radDefaultHTTPTimeout      = 5
	radDefaultHTTPPasswordType = "plain"
	// 设备凭证认证源已从 UI 下线，遗留 device 值按 local 展示
	radAuthSourceDevice = "device"
)

// radiusPage 是认证源管理页视图：北向认证源选择 + 各认证源参数 + 本地用户表。
type radiusPage struct {
	pageBase
	Cfg      *frpsclient.RadiusGlobalConfig
	CfgErr   string // 全局配置拉取失败提示（frps_helper 不可达等）
	FlashOK  string
	FlashErr string
	// LocalUsers 本地 RADIUS 用户列表（auth_source=local 时使用）
	LocalUsers []frpsclient.LocalUserItem
	LocalTotal int
}

// handleRadiusPage 渲染认证源管理页（/auth-source）。
func (a *App) handleRadiusPage(c *gin.Context) {
	u, _ := auth.CurrentUser(c)
	pg := radiusPage{
		pageBase: pageBase{ActiveMenu: "radius", CurrentUser: u, Config: a.Cfg},
		FlashOK:  c.Query("ok"),
		FlashErr: c.Query("err"),
	}

	cfg, err := a.Frps.GetRadiusConfig(c.Request.Context())
	if err != nil {
		a.Log.Error("get radius config failed", "err", err)
		pg.CfgErr = "获取全局 RADIUS 配置失败：" + err.Error()
	} else {
		pg.Cfg = radiusDisplayCfg(cfg)
		// 保存失败重定向时保留用户当时选中的认证源，避免 TAB 跳回已保存的旧值
		if src := c.Query("src"); src != "" {
			switch src {
			case "local", "radius", "http", "ldap":
				pg.Cfg.AuthSource = src
			case "device":
				pg.Cfg.AuthSource = "local"
			}
		}
	}

	// 本地 RADIUS 用户列表（用于认证源管理 TAB；拉取失败仅影响展示）
	if lu, err := a.Frps.ListLocalUsers(c.Request.Context(), 1, 200, "", ""); err == nil && lu != nil {
		pg.LocalUsers = lu.List
		pg.LocalTotal = lu.Total
	}

	if rerr := a.Render.Render(c.Writer, "pages/radius/index", pg); rerr != nil {
		a.Log.Error("render radius page failed", "err", rerr)
	}
}

// radiusDisplayCfg 将未配置项以缺省值填充用于表单展示（提交后显式落盘）。
func radiusDisplayCfg(cfg *frpsclient.RadiusGlobalConfig) *frpsclient.RadiusGlobalConfig {
	if cfg.AuthListenPort == 0 {
		cfg.AuthListenPort = radDefaultAuthPort
	}
	if cfg.AcctListenPort == 0 {
		cfg.AcctListenPort = radDefaultAcctPort
	}
	if cfg.AuthTimeout == 0 {
		cfg.AuthTimeout = radDefaultAuthTimeout
	}
	if cfg.AcctTimeout == 0 {
		cfg.AcctTimeout = radDefaultAcctTimeout
	}
	if cfg.AcctSessionTimeout == 0 {
		cfg.AcctSessionTimeout = radDefaultSessionTimeout
	}
	if cfg.AcctCleanupInterval == 0 {
		cfg.AcctCleanupInterval = radDefaultCleanupInterval
	}
	if cfg.UpstreamAuthPort == 0 {
		cfg.UpstreamAuthPort = radDefaultUpstreamAuthPort
	}
	if cfg.UpstreamAcctPort == 0 {
		cfg.UpstreamAcctPort = radDefaultUpstreamAcctPort
	}
	if cfg.AuthSource == "" {
		cfg.AuthSource = radDefaultAuthSource
	}
	// 设备凭证认证源已从 UI 下线：历史 device 配置按 local 展示。
	if cfg.AuthSource == radAuthSourceDevice {
		cfg.AuthSource = "local"
	}
	if cfg.LDAPPort == 0 {
		cfg.LDAPPort = radDefaultLDAPPort
	}
	if cfg.LDAPUserFilter == "" {
		cfg.LDAPUserFilter = radDefaultLDAPUserFilter
	}
	if cfg.LDAPTimeout == 0 {
		cfg.LDAPTimeout = radDefaultLDAPTimeout
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = radDefaultHTTPTimeout
	}
	if cfg.HTTPPasswordType == "" {
		cfg.HTTPPasswordType = radDefaultHTTPPasswordType
	}
	return cfg
}

// handleRadiusGlobalUpdate 保存认证源配置（POST /auth-source）。
func (a *App) handleRadiusGlobalUpdate(c *gin.Context) {
	back := "/auth-source"
	src := c.PostForm("auth_source")
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) {
		c.Redirect(http.StatusFound, back+"?err="+url.QueryEscape(msg)+"&src="+url.QueryEscape(src))
	}

	// 只提交认证源相关字段；服务监听/密钥/超时/宣告参数在「RADIUS 服务」页维护。
	// 整份配置回传会把未配置的 auth_listen_addr 等零值当成显式赋值而被 frps_helper 拒绝。
	cfg := &frpsclient.RadiusGlobalConfig{
		AuthSource: c.PostForm("auth_source"),
	}
	if cfg.AuthSource == "" {
		cfg.AuthSource = radDefaultAuthSource
	}
	cfg.UpstreamServer = c.PostForm("upstream_server")
	cfg.UpstreamAuthPort = atoiDefault(c.PostForm("upstream_auth_port"), radDefaultUpstreamAuthPort)
	cfg.UpstreamAcctPort = atoiDefault(c.PostForm("upstream_acct_port"), radDefaultUpstreamAcctPort)
	cfg.UpstreamSecret = c.PostForm("upstream_secret")
	cfg.UpstreamStopOnTimeout = c.PostForm("upstream_stop_on_timeout") == "1"
	cfg.LDAPServer = c.PostForm("ldap_server")
	cfg.LDAPPort = atoiDefault(c.PostForm("ldap_port"), radDefaultLDAPPort)
	cfg.LDAPBindDN = c.PostForm("ldap_bind_dn")
	cfg.LDAPBindPassword = c.PostForm("ldap_bind_password")
	cfg.LDAPBaseDN = c.PostForm("ldap_base_dn")
	cfg.LDAPUserFilter = c.PostForm("ldap_user_filter")
	if cfg.LDAPUserFilter == "" {
		cfg.LDAPUserFilter = radDefaultLDAPUserFilter
	}
	cfg.LDAPTimeout = atoiDefault(c.PostForm("ldap_timeout"), radDefaultLDAPTimeout)
	cfg.LDAPUseTLS = c.PostForm("ldap_use_tls") == "1"
	cfg.HTTPAuthURL = c.PostForm("http_auth_url")
	cfg.HTTPAcctURL = c.PostForm("http_acct_url")
	cfg.HTTPPasswordType = c.PostForm("http_password_type")
	if cfg.HTTPPasswordType == "" {
		cfg.HTTPPasswordType = radDefaultHTTPPasswordType
	}
	cfg.HTTPSharedSecret = c.PostForm("http_shared_secret")
	cfg.HTTPTimeout = atoiDefault(c.PostForm("http_timeout"), radDefaultHTTPTimeout)
	cfg.HTTPTLSVerify = c.PostForm("http_tls_verify") == "1"
	cfg.NASIP = c.PostForm("nas_ip")

	if _, err := a.Frps.UpdateRadiusAuthSource(c.Request.Context(), cfg); err != nil {
		a.Log.Error("update radius config failed", "err", err)
		errMsg("保存失败：" + err.Error())
		return
	}
	okMsg("认证源配置已保存并即时生效")
}

// radiusServicePage 是 RADIUS 服务页视图：监听开关/地址端口、全局密钥、超时、宣告参数。
type radiusServicePage struct {
	pageBase
	Cfg      *frpsclient.RadiusGlobalConfig
	CfgErr   string
	FlashOK  string
	FlashErr string
}

// handleRadiusServicePage 渲染「RADIUS 服务」页（GET /radius/service）。
func (a *App) handleRadiusServicePage(c *gin.Context) {
	u, _ := auth.CurrentUser(c)
	pg := radiusServicePage{
		pageBase: pageBase{ActiveMenu: "radius-service", CurrentUser: u, Config: a.Cfg},
		FlashOK:  c.Query("ok"),
		FlashErr: c.Query("err"),
	}
	cfg, err := a.Frps.GetRadiusConfig(c.Request.Context())
	if err != nil {
		a.Log.Error("get radius config failed", "err", err)
		pg.CfgErr = "获取全局 RADIUS 配置失败：" + err.Error()
	} else {
		pg.Cfg = radiusDisplayCfg(cfg)
	}
	if rerr := a.Render.Render(c.Writer, "pages/radius_service/index", pg); rerr != nil {
		a.Log.Error("render radius service page failed", "err", rerr)
	}
}

// handleRadiusServiceUpdate 保存「RADIUS 服务」配置（POST /radius/service）。
// 提交后重定向回「系统设置」页并停留 RADIUS 服务 TAB。
func (a *App) handleRadiusServiceUpdate(c *gin.Context) {
	back := "/settings"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+url.QueryEscape(msg)) }

	cfg, err := a.Frps.GetRadiusConfig(c.Request.Context())
	if err != nil {
		a.Log.Error("get radius config before service update failed", "err", err)
		errMsg("保存失败：获取当前配置失败 " + err.Error())
		return
	}

	cfg.AuthEnable = c.PostForm("auth_enable") == "1"
	cfg.AcctEnable = c.PostForm("acct_enable") == "1"
	cfg.AuthListenAddr = c.PostForm("auth_listen_addr")
	cfg.AuthListenPort = atoiDefault(c.PostForm("auth_listen_port"), radDefaultAuthPort)
	cfg.AcctListenAddr = c.PostForm("acct_listen_addr")
	cfg.AcctListenPort = atoiDefault(c.PostForm("acct_listen_port"), radDefaultAcctPort)
	cfg.AuthSecret = c.PostForm("auth_secret")
	cfg.AcctSecret = c.PostForm("acct_secret")
	cfg.AuthTimeout = atoiDefault(c.PostForm("auth_timeout"), radDefaultAuthTimeout)
	cfg.AcctTimeout = atoiDefault(c.PostForm("acct_timeout"), radDefaultAcctTimeout)
	cfg.AcctSessionTimeout = atoiDefault(c.PostForm("acct_session_timeout"), radDefaultSessionTimeout)
	cfg.AcctCleanupInterval = atoiDefault(c.PostForm("acct_cleanup_interval"), radDefaultCleanupInterval)
	cfg.AdvertisedServer = c.PostForm("advertised_server")
	cfg.AdvertisedAuthPort = atoiDefault(c.PostForm("advertised_auth_port"), 0)
	cfg.AdvertisedAcctPort = atoiDefault(c.PostForm("advertised_acct_port"), 0)

	if _, err := a.Frps.UpdateRadiusConfig(c.Request.Context(), cfg); err != nil {
		a.Log.Error("update radius service config failed", "err", err)
		errMsg("保存失败：" + err.Error())
		return
	}
	okMsg("RADIUS 服务配置已保存并即时生效")
}

// handleRadiusDeviceUpdate 保存单台设备的 RADIUS 参数（POST /devices/:id/radius）。
// 表单整行提交：空地址/0端口显式清空；勾选"重新生成密钥"时由本端生成 24 位随机密钥。
func (a *App) handleRadiusDeviceUpdate(c *gin.Context) {
	deviceID := c.Param("id")
	back := "/devices/" + url.PathEscape(deviceID)
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+url.QueryEscape(msg)) }

	upd := frpsclient.DeviceRadiusUpdate{
		RadAuthServer: strPtr(c.PostForm("rad_auth_server")),
		RadAcctServer: strPtr(c.PostForm("rad_acct_server")),
		RadNasIP:      strPtr(c.PostForm("rad_nas_ip")),
	}
	authPort := atoiDefault(c.PostForm("rad_auth_port"), 0)
	acctPort := atoiDefault(c.PostForm("rad_acct_port"), 0)
	upd.RadAuthPort = &authPort
	upd.RadAcctPort = &acctPort

	if c.PostForm("regen_auth_secret") == "1" {
		sec, err := genRadSecret()
		if err != nil {
			a.Log.Error("gen radius secret failed", "err", err)
			errMsg("生成认证密钥失败：" + err.Error())
			return
		}
		upd.RadAuthSecret = &sec
	} else {
		upd.RadAuthSecret = strPtr(c.PostForm("rad_auth_secret"))
	}
	if c.PostForm("regen_acct_secret") == "1" {
		sec, err := genRadSecret()
		if err != nil {
			a.Log.Error("gen radius secret failed", "err", err)
			errMsg("生成记账密钥失败：" + err.Error())
			return
		}
		upd.RadAcctSecret = &sec
	} else {
		upd.RadAcctSecret = strPtr(c.PostForm("rad_acct_secret"))
	}

	if _, err := a.Frps.UpdateDeviceRadius(c.Request.Context(), deviceID, &upd); err != nil {
		a.Log.Error("update device radius failed", "device_id", deviceID, "err", err)
		errMsg("保存设备 RADIUS 参数失败：" + err.Error())
		return
	}
	okMsg("设备 " + deviceID + " RADIUS 参数已保存")
}

// strPtr 表单值转指针（空串保留为空串指针，表达"显式清空"语义）
func strPtr(s string) *string { return &s }

// radSecretLen 每设备 RADIUS 密钥长度（与 frps_helper 一致：24，字母数字）
const radSecretLen = 24

// genRadSecret 生成随机共享密钥（仅大小写字母+数字，避免设备侧转义问题）
func genRadSecret() (string, error) {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, radSecretLen)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		buf[i] = charset[n.Int64()]
	}
	return string(buf), nil
}

// radiusPageURL 构造 /radius 分页链接（保留 keyword）。
func radiusPageURL(n int, keyword string) string {
	q := url.Values{}
	if keyword != "" {
		q.Set("keyword", keyword)
	}
	q.Set("page", strconv.Itoa(n))
	return "/auth-source?" + q.Encode()
}

// radiusPageLinks 构造分页条目（复用 devices 的 pageLink 结构）。
func radiusPageLinks(page, total int, keyword string) []pageLink {
	links := make([]pageLink, 0, 10)
	links = append(links, pageLink{Label: "«", URL: radiusPageURL(page-1, keyword), Disabled: page <= 1})
	start := page - 2
	if start < 1 {
		start = 1
	}
	end := page + 2
	if end > total {
		end = total
	}
	if start > 1 {
		links = append(links, pageLink{Label: "1", URL: radiusPageURL(1, keyword)})
		if start > 2 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
	}
	for i := start; i <= end; i++ {
		links = append(links, pageLink{Label: strconv.Itoa(i), URL: radiusPageURL(i, keyword), Active: i == page})
	}
	if end < total {
		if end < total-1 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
		links = append(links, pageLink{Label: strconv.Itoa(total), URL: radiusPageURL(total, keyword)})
	}
	links = append(links, pageLink{Label: "»", URL: radiusPageURL(page+1, keyword), Disabled: page >= total})
	return links
}
