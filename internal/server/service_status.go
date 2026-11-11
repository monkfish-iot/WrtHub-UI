package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"wrthub-ui/internal/config"
	"wrthub-ui/internal/frpsclient"
)

// serviceStatusView 是概况页「服务运行状态」表格的一行。
type serviceStatusView struct {
	Name       string
	OK         bool
	StatusText string
	Version    string
	Uptime     string
	Memory     string
	Ports      string
	Detail     string
	Error      string
}

// portalHealth 是 portal-server GET /healthz 的响应。
type portalHealth struct {
	Status           string    `json:"status"`
	Version          string    `json:"version"`
	StartedAt        time.Time `json:"started_at"`
	UptimeSeconds    int64     `json:"uptime_seconds"`
	Goroutines       int       `json:"goroutines"`
	MemoryAllocBytes uint64    `json:"memory_alloc_bytes"`
	MemorySysBytes   uint64    `json:"memory_sys_bytes"`
	Templates        []string  `json:"templates"`
	TemplateRoot     string    `json:"template_root"`
	DefaultTemplate  string    `json:"default_template"`
	XAccel           bool      `json:"x_accel"`
}

// fetchPortalHealth 请求 portal-server /healthz，超时由配置控制。
func fetchPortalHealth(ctx context.Context, cfg config.PortalServerProbeConfig) (*portalHealth, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/healthz"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("portal-server http %d", resp.StatusCode)
	}
	var h portalHealth
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, fmt.Errorf("decode portal health: %w", err)
	}
	if h.Status != "ok" {
		return nil, fmt.Errorf("portal-server status %q", h.Status)
	}
	return &h, nil
}

// selfServiceStatus 返回 WrtHub-UI 自身运行信息。
func (a *App) selfServiceStatus(now time.Time) serviceStatusView {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	return serviceStatusView{
		Name:       "WrtHub-UI",
		OK:         true,
		StatusText: "运行中",
		Version:    buildVersion(),
		Uptime:     formatUptime(int64(now.Sub(a.StartedAt).Seconds())),
		Memory:     formatMemory(ms.Sys),
		Ports:      formatPort(a.Cfg.Server.Addr, "8080"),
		Detail:     fmt.Sprintf("管理端 · 数据库 %s · 认证模式 %s", a.Cfg.Storage.Driver, a.Cfg.Auth.Mode),
	}
}

// serviceFrpsStatus 返回 FRPS 运行状态（数据经 frps_helper 聚合）。
func serviceFrpsStatus(st *frpsclient.SystemStatus, err error) serviceStatusView {
	if err != nil || st == nil {
		msg := "frps 状态获取失败（依赖 frps-helper 状态接口）"
		if err != nil {
			msg = truncateText(err.Error(), 120)
		}
		return serviceStatusView{
			Name:       "frps",
			StatusText: "不可达",
			Version:    "—",
			Uptime:     "—",
			Memory:     "—",
			Ports:      "7000 代理 / 9800 管理",
			Error:      msg,
		}
	}

	frps := st.Frps
	v := serviceStatusView{
		Name:    "frps",
		Version: "—",
		Uptime:  "—",
		Memory:  "—",
		Ports:   "7000 代理 / 9800 管理",
	}
	if frps.BindPort > 0 {
		v.Ports = fmt.Sprintf("%d 代理 / 9800 管理", frps.BindPort)
	}
	if !frps.Reachable {
		v.StatusText = "不可达"
		v.Error = "FRPS dashboard 不可达"
		if frps.Error != "" {
			v.Error = truncateText(frps.Error, 120)
		}
		return v
	}

	v.OK = true
	v.StatusText = "运行中"
	v.Version = frps.Version
	v.Detail = fmt.Sprintf("在线客户端 %d · 当前连接 %d · 流量 ↑%s ↓%s",
		frps.OnlineClients, frps.CurConns,
		formatTraffic(frps.TotalTrafficIn), formatTraffic(frps.TotalTrafficOut))
	return v
}

// serviceFrpsHelperStatus 返回 frps_helper 进程运行状态。
func serviceFrpsHelperStatus(st *frpsclient.SystemStatus, err error) serviceStatusView {
	v := serviceStatusView{
		Name:    "frps-helper",
		Version: "—",
		Uptime:  "—",
		Memory:  "—",
		Ports:   "8089 / 6999 / 7443",
	}
	if err != nil || st == nil {
		v.StatusText = "不可达"
		if err != nil {
			v.Error = truncateText(err.Error(), 120)
		}
		return v
	}

	radiusRunning := false
	if st.Runtime.Radius != nil {
		radiusRunning, _ = st.Runtime.Radius["running"].(bool)
	}
	radiusState := "RADIUS 未运行"
	if radiusRunning {
		radiusState = "RADIUS 运行中"
	}
	dbState := "数据库异常"
	if st.Runtime.Database.OK {
		dbState = "数据库正常"
	}

	v.OK = true
	v.StatusText = "运行中"
	v.Version = st.Service.Version
	v.Uptime = formatUptime(st.Service.UptimeSeconds)
	v.Memory = formatMemory(st.Service.MemorySysBytes)
	if st.Runtime.AuthPort > 0 || st.Runtime.DevicePort > 0 || st.Runtime.ManagePort > 0 {
		v.Ports = fmt.Sprintf("%d / %d / %d", st.Runtime.AuthPort, st.Runtime.DevicePort, st.Runtime.ManagePort)
	}
	v.Detail = fmt.Sprintf("在线设备 %d · 认证客户端 %d · %s · %s",
		st.Runtime.OnlineDevices, st.Runtime.ClientCount, radiusState, dbState)
	return v
}

// servicePortalStatus 返回 portal-server 运行状态。
func servicePortalStatus(h *portalHealth, err error, baseURL string) serviceStatusView {
	v := serviceStatusView{
		Name:    "portal-server",
		Version: "—",
		Uptime:  "—",
		Memory:  "—",
		Ports:   formatPortFromURL(baseURL, "19090"),
	}
	if err != nil || h == nil {
		v.StatusText = "不可达"
		if err != nil {
			v.Error = truncateText(err.Error(), 120)
		}
		return v
	}

	mode := "base 模式"
	if h.XAccel {
		mode = "X-Accel 模式"
	}
	v.OK = true
	v.StatusText = "运行中"
	v.Version = h.Version
	v.Uptime = formatUptime(h.UptimeSeconds)
	v.Memory = formatMemory(h.MemorySysBytes)
	v.Detail = fmt.Sprintf("模板 %d 个 · 兜底 %s · %s", len(h.Templates), h.DefaultTemplate, mode)
	return v
}

// buildVersion 返回便于展示的版本号：发布版本原样返回；
// 开发构建（伪版本/含 dirty）回退 VCS 短提交，避免表格出现超长版本串。
func buildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" && len(v) <= 20 && !strings.Contains(v, "+dirty") {
		return v
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 8 {
			return "git " + s.Value[:8]
		}
	}
	return "dev"
}

func formatUptime(sec int64) string {
	if sec <= 0 {
		return "—"
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%d天%d小时", d, h)
	case h > 0:
		return fmt.Sprintf("%d小时%d分", h, m)
	case m > 0:
		return fmt.Sprintf("%d分%d秒", m, sec%60)
	default:
		return fmt.Sprintf("%d秒", sec)
	}
}

func formatMemory(b uint64) string {
	if b == 0 {
		return "—"
	}
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	default:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(kb))
	}
}

func formatTraffic(b int64) string {
	if b < 0 {
		return "—"
	}
	return formatMemory(uint64(b))
}

// formatPort 把监听地址（":8080"、"0.0.0.0:8080"）转换为纯端口展示。
func formatPort(addr, fallback string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fallback
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 && i < len(addr)-1 {
		return addr[i+1:]
	}
	return addr
}

// formatPortFromURL 从 base_url 提取端口。
func formatPortFromURL(raw, fallback string) string {
	u, err := url.Parse(raw)
	if err == nil && u.Port() != "" {
		return u.Port()
	}
	return fallback
}

// truncateText 按字符截断文本，避免表格中出现超长错误信息。
func truncateText(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "..."
}
