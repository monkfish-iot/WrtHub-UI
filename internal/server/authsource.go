package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/frpsclient"
)

// 认证源标签（与 webauthd AUTH_SOURCE_* 对齐）。
var authSourceLabels = map[string]string{
	"local":  "本地认证",
	"radius": "RADIUS",
	"http":   "HTTP",
	"ldap":   "LDAP",
}

// 认证源枚举对应的 UCI 取值（webauthd.generic.auth_source）。
var authSourceCode = map[string]string{
	"local":  "0",
	"radius": "1",
	"http":   "2",
	"ldap":   "3",
}

// authRadiusForm 是 RADIUS 认证源参数视图（密钥只回显"是否已设置"）。
type authRadiusForm struct {
	AuthServer    string
	AuthPort      string
	AcctServer    string
	AcctPort      string
	AcctInterval  string
	AuthSecretSet bool
	AcctSecretSet bool
}

// authHTTPForm 是 HTTP 认证源参数视图。
type authHTTPForm struct {
	AuthURL         string
	AcctURL         string
	PasswordType    string // plain / md5
	Timeout         string
	TLSVerify       bool
	SharedSecretSet bool
}

// authLDAPForm 是 LDAP 认证源参数视图。
type authLDAPForm struct {
	Server          string
	Port            string
	BindDN          string
	BindPasswordSet bool
}

// authSourceView 是"认证源配置（按设备）"卡片视图。
type authSourceView struct {
	Devices    []frpsclient.Device
	DeviceID   string
	DeviceName string
	Online     bool
	Source     string // local / radius / http / ldap
	Radius     authRadiusForm
	HTTP       authHTTPForm
	LDAP       authLDAPForm
	Err        string
}

// loadAuthSource 拉取设备列表并读取选中设备的 webauthd 认证源配置。
// devID 为空时默认选中第一台设备；设备离线/不可达时仅返回提示，不阻断页面其他部分。
func (a *App) loadAuthSource(ctx context.Context, devID string) *authSourceView {
	v := &authSourceView{Source: "local"}

	res, err := a.Frps.ListDevices(ctx, frpsclient.ListOptions{Page: 1, PageSize: 100})
	if err != nil {
		v.Err = "获取设备列表失败：" + err.Error()
		return v
	}
	if res != nil {
		v.Devices = res.Items
	}
	if devID == "" && len(v.Devices) > 0 {
		devID = v.Devices[0].DeviceID
	}
	v.DeviceID = devID

	var dev *frpsclient.Device
	for i := range v.Devices {
		if v.Devices[i].DeviceID == devID {
			dev = &v.Devices[i]
			break
		}
	}
	if dev == nil {
		if devID != "" {
			v.Err = "设备不存在或不可见：" + devID
		}
		return v
	}
	v.DeviceName = dev.DeviceName
	v.Online = dev.SessionID != ""
	if !v.Online {
		v.Err = msgDeviceUnreachable
		return v
	}

	secs, err := fetchUciSections(ctx, a, dev.SessionID, "webauthd")
	if err != nil {
		a.Log.Warn("read webauthd config failed", "device_id", devID, "err", err)
		v.Err = "读取认证源配置失败：" + friendlyDeviceErr(err)
		return v
	}
	gen := pickSection(secs, "generic")
	if gen == nil {
		v.Err = "设备缺少 webauthd generic 配置段（该设备可能未启用 Portal 认证）"
		return v
	}
	v.Source = normAuthSource(secField(*gen, "auth_source"))

	if r := pickSection(secs, "radius"); r != nil {
		v.Radius = authRadiusForm{
			AuthServer:    secField(*r, "primary_auth_server"),
			AuthPort:      secField(*r, "primary_auth_port"),
			AcctServer:    secField(*r, "primary_acct_server"),
			AcctPort:      secField(*r, "primary_acct_port"),
			AcctInterval:  secField(*r, "acct_interval"),
			AuthSecretSet: secField(*r, "primary_auth_secret") != "",
			AcctSecretSet: secField(*r, "primary_acct_secret") != "",
		}
	}
	if h := pickSection(secs, "http_auth"); h != nil {
		v.HTTP = authHTTPForm{
			AuthURL:         secField(*h, "auth_url"),
			AcctURL:         secField(*h, "acct_url"),
			PasswordType:    secField(*h, "password_type"),
			Timeout:         secField(*h, "timeout"),
			TLSVerify:       uciTruthy(secField(*h, "tls_verify")),
			SharedSecretSet: secField(*h, "shared_secret") != "",
		}
	}
	if v.HTTP.PasswordType == "" {
		v.HTTP.PasswordType = "plain"
	}
	if l := pickSection(secs, "ldap"); l != nil {
		v.LDAP = authLDAPForm{
			Server:          secField(*l, "server"),
			Port:            secField(*l, "port"),
			BindDN:          secField(*l, "bind_dn"),
			BindPasswordSet: secField(*l, "bind_password") != "",
		}
	}
	return v
}

// handleAuthSourceUpdate 保存选中设备的认证源与对应参数（POST /radius/auth-source/:id）。
//
// 流程：校验参数 → 写对应 UCI 段（密钥留空=保留）→ 调 mk.portal base_set 切换 auth_source
// 并触发 webauthd 异步重启；base_set 不可用时回退 uci.set + commit，提示需手动重启。
// LDAP 当前固件仅占位：保存参数但不切换认证源，避免设备直接无法认证。
func (a *App) handleAuthSourceUpdate(c *gin.Context) {
	devID := c.Param("id")
	back := "/auth-source?dev=" + url.QueryEscape(devID)
	okMsg := func(msg string) { redirectFlash(c, back, "ok", msg) }
	errMsg := func(msg string) { redirectFlash(c, back, "err", msg) }

	ctx := c.Request.Context()
	dev, err := a.Frps.GetDevice(ctx, devID)
	if err != nil {
		a.Log.Error("auth source: get device failed", "device_id", devID, "err", err)
		errMsg("设备不存在或不可见：" + err.Error())
		return
	}
	if dev.SessionID == "" {
		errMsg(msgDeviceUnreachable)
		return
	}

	secs, err := fetchUciSections(ctx, a, dev.SessionID, "webauthd")
	if err != nil {
		a.Log.Error("auth source: read webauthd failed", "device_id", devID, "err", err)
		errMsg("读取设备认证源配置失败：" + friendlyDeviceErr(err))
		return
	}
	gen := pickSection(secs, "generic")
	if gen == nil {
		errMsg("设备缺少 webauthd generic 配置段，无法保存")
		return
	}

	src := strings.ToLower(strings.TrimSpace(c.PostForm("auth_source")))
	code, ok := authSourceCode[src]
	if !ok {
		errMsg("认证源参数非法")
		return
	}

	var (
		secName = "radius"
		secType = "radius"
		values  map[string]any
		verr    error
	)
	switch src {
	case "radius":
		values, verr = radiusAuthValues(c)
	case "http":
		secName, secType = "http_auth", "http_auth"
		values, verr = httpAuthValues(c)
	case "ldap":
		secName, secType = "ldap", "ldap"
		values, verr = ldapAuthValues(c)
	case "local":
		values = nil
	}
	if verr != nil {
		errMsg(verr.Error())
		return
	}

	commit := func() error {
		return a.Ubus.CallWriteInto(ctx, dev.SessionID, "uci", "commit", &map[string]any{}, map[string]any{"config": "webauthd"})
	}

	// 参数落盘（local 无参数可写）。
	if values != nil {
		exists := pickSection(secs, secName) != nil
		if err := writeUciSection(ctx, a, dev.SessionID, "webauthd", secName, secType, values, exists); err != nil {
			a.Log.Error("auth source: write params failed", "device_id", devID, "section", secName, "err", err)
			errMsg("保存认证源参数失败：" + friendlyDeviceErr(err))
			return
		}
	}

	// LDAP 固件仅占位：保存参数但不切换认证源。
	if src == "ldap" {
		if err := commit(); err != nil {
			errMsg("保存失败：" + friendlyDeviceErr(err))
			return
		}
		okMsg("LDAP 参数已保存；当前固件尚未实现 LDAP 认证，未切换认证源（仅表单占位）")
		return
	}

	// 切换认证源：走厂商 base_set（提交 + webauthd 异步重启）。
	var resp struct {
		WeAuthdRestart string `json:"webauthd_restart"`
	}
	if err := a.Ubus.CallWriteInto(ctx, dev.SessionID, "mk.portal", "base_set", &resp, baseSetArgs(*gen, code)); err != nil {
		a.Log.Warn("mk.portal base_set failed, fallback to uci.set", "device_id", devID, "err", err)
		if err2 := writeUciSection(ctx, a, dev.SessionID, "webauthd", gen.Name, "generic",
			map[string]any{"auth_source": code}, true); err2 != nil {
			errMsg("保存失败：" + friendlyDeviceErr(err2))
			return
		}
		if err2 := commit(); err2 != nil {
			errMsg("保存失败：" + friendlyDeviceErr(err2))
			return
		}
		okMsg("认证源与参数已保存；设备固件不支持自动重启 webauthd，请手动重启后生效")
		return
	}

	msg := "认证源已切换为「" + authSourceLabels[src] + "」，参数已保存"
	if resp.WeAuthdRestart != "" {
		msg += "；" + resp.WeAuthdRestart
	}
	okMsg(msg)
}

// radiusAuthValues 解析并校验 RADIUS 参数表单。
func radiusAuthValues(c *gin.Context) (map[string]any, error) {
	authServer := strings.TrimSpace(c.PostForm("rad_auth_server"))
	if authServer == "" {
		return nil, fmt.Errorf("RADIUS 认证服务器地址不能为空")
	}
	authPort, err := uciPort(c.PostForm("rad_auth_port"), 1812, "RADIUS 认证端口")
	if err != nil {
		return nil, err
	}
	acctPort, err := uciPort(c.PostForm("rad_acct_port"), 1813, "RADIUS 记账端口")
	if err != nil {
		return nil, err
	}
	interval, err := uciInt(c.PostForm("acct_interval"), 60, 1, 86400, "记账间隔")
	if err != nil {
		return nil, err
	}

	values := map[string]any{
		"primary_auth_server": authServer,
		"primary_auth_port":   authPort,
		"primary_acct_server": strings.TrimSpace(c.PostForm("rad_acct_server")),
		"primary_acct_port":   acctPort,
		"acct_interval":       interval,
	}
	if s := c.PostForm("rad_auth_secret"); s != "" {
		if err := validateSecret(s, "认证密钥"); err != nil {
			return nil, err
		}
		values["primary_auth_secret"] = s
	}
	if s := c.PostForm("rad_acct_secret"); s != "" {
		if err := validateSecret(s, "记账密钥"); err != nil {
			return nil, err
		}
		values["primary_acct_secret"] = s
	}
	return values, nil
}

// httpAuthValues 解析并校验 HTTP 认证源参数表单（参照 webauthd http_auth 段设计）。
func httpAuthValues(c *gin.Context) (map[string]any, error) {
	authURL := strings.TrimSpace(c.PostForm("http_auth_url"))
	if authURL == "" {
		return nil, fmt.Errorf("HTTP 认证 URL 不能为空")
	}
	if !strings.HasPrefix(authURL, "http://") && !strings.HasPrefix(authURL, "https://") {
		return nil, fmt.Errorf("HTTP 认证 URL 必须以 http:// 或 https:// 开头")
	}
	if len(authURL) > 500 {
		return nil, fmt.Errorf("HTTP 认证 URL 不能超过 500 个字符")
	}
	acctURL := strings.TrimSpace(c.PostForm("http_acct_url"))
	if acctURL != "" && !strings.HasPrefix(acctURL, "http://") && !strings.HasPrefix(acctURL, "https://") {
		return nil, fmt.Errorf("HTTP 记账 URL 必须以 http:// 或 https:// 开头")
	}
	passType := strings.TrimSpace(c.PostForm("http_password_type"))
	if passType != "md5" {
		passType = "plain"
	}
	timeout, err := uciInt(c.PostForm("http_timeout"), 5, 1, 60, "HTTP 请求超时")
	if err != nil {
		return nil, err
	}

	tlsVerify := "0"
	if c.PostForm("http_tls_verify") == "1" {
		tlsVerify = "1"
	}
	values := map[string]any{
		"auth_url":      authURL,
		"acct_url":      acctURL,
		"password_type": passType,
		"timeout":       timeout,
		"tls_verify":    tlsVerify,
	}
	if s := c.PostForm("http_shared_secret"); s != "" {
		if err := validateSecret(s, "共享密钥"); err != nil {
			return nil, err
		}
		values["shared_secret"] = s
	}
	return values, nil
}

// ldapAuthValues 解析并校验 LDAP 参数表单（当前固件仅占位）。
func ldapAuthValues(c *gin.Context) (map[string]any, error) {
	server := strings.TrimSpace(c.PostForm("ldap_server"))
	if server == "" {
		return nil, fmt.Errorf("LDAP 服务器地址不能为空")
	}
	port, err := uciPort(c.PostForm("ldap_port"), 389, "LDAP 端口")
	if err != nil {
		return nil, err
	}
	values := map[string]any{
		"server":  server,
		"port":    port,
		"bind_dn": strings.TrimSpace(c.PostForm("ldap_bind_dn")),
	}
	if s := c.PostForm("ldap_bind_password"); s != "" {
		values["bind_password"] = s
	}
	return values, nil
}

// normAuthSource 将 webauthd.auth_source 原始值归一化为 local/radius/http/ldap。
func normAuthSource(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "radius":
		return "radius"
	case "2", "http", "https":
		return "http"
	case "3", "ldap":
		return "ldap"
	default:
		return "local"
	}
}

// pickSection 按 section 名优先、类型兜底查找（webauthd generic 段类型为 base、名为 generic）。
func pickSection(secs []uciSection, name string) *uciSection {
	for i := range secs {
		if secs[i].Name == name {
			return &secs[i]
		}
	}
	for i := range secs {
		if secs[i].Type == name {
			return &secs[i]
		}
	}
	return nil
}

// sectionMap 将 section 选项转为键值 map。
func sectionMap(s uciSection) map[string]string {
	m := make(map[string]string, len(s.Fields))
	for _, f := range s.Fields {
		m[f.Key] = f.Value
	}
	return m
}

// baseSetArgs 用当前 generic 段值 + 新认证源组装 mk.portal base_set 参数。
// base_set 会整段重写 generic 并提交、按需重启 webauthd，故必须回传既有字段避免被清空。
func baseSetArgs(gen uciSection, code string) map[string]any {
	m := sectionMap(gen)
	// 整型参数须按 int 传入：rpcd 方法表把 idle_timeout/capture_dns/mac_auth 等声明为整型，
	// 传字符串会因参数类型不符被拒。
	args := map[string]any{
		"portal_interface": m["portal_interface"],
		"layer2_ifname":    m["layer2_ifname"],
		"idle_timeout":     atoiDefault(m["idle_timeout"], 120),
		"capture_dns":      atoiDefault(m["capture_dns"], 1),
		"mac_auth":         atoiDefault(m["mac_auth"], 1),
		"mac_auth_format":  atoiDefault(m["mac_auth_format"], 1),
		"cloud_config":     atoiDefault(m["cloud_config"], 0),
		"auth_source":      code,
	}
	if m["max_users"] != "" {
		args["max_users"] = m["max_users"]
	}
	return args
}

// writeUciSection 写 UCI 段：已存在走 set，缺失走 add（指定 name=type 便于后续读取）。
func writeUciSection(ctx context.Context, a *App, sessionID, config, name, typ string, values map[string]any, exists bool) error {
	if exists {
		return a.Ubus.CallWriteInto(ctx, sessionID, "uci", "set", &map[string]any{},
			map[string]any{"config": config, "section": name, "values": values})
	}
	return a.Ubus.CallWriteInto(ctx, sessionID, "uci", "add", &map[string]any{},
		map[string]any{"config": config, "type": typ, "name": name, "values": values})
}

// redirectFlash 重定向并携带 ok=/err= 回显（back 可能已带 query）。
func redirectFlash(c *gin.Context, back, key, msg string) {
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	c.Redirect(http.StatusFound, back+sep+key+"="+url.QueryEscape(msg))
}

// uciPort 端口校验（空值用默认端口）。
func uciPort(raw string, def int, label string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return strconv.Itoa(def), nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%s必须是 1–65535 的整数", label)
	}
	return strconv.Itoa(n), nil
}

// uciInt 整数校验（空值用默认值）。
func uciInt(raw string, def, min, max int, label string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return strconv.Itoa(def), nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return "", fmt.Errorf("%s必须是 %d–%d 的整数", label, min, max)
	}
	return strconv.Itoa(n), nil
}

// validateSecret 密钥校验（≤64 字符，禁换行/引号/反斜杠，避免 UCI 注入）。
func validateSecret(s, label string) error {
	if len(s) > 64 {
		return fmt.Errorf("%s不能超过 64 个字符", label)
	}
	if strings.ContainsAny(s, "\n\r\"\\") {
		return fmt.Errorf("%s不能包含换行、引号或反斜杠", label)
	}
	return nil
}
