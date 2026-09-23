package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/frpsclient"
)

// netDevRow 是网络接口表格的单行视图（来自 ubus luci-rpc getNetworkDevices）。
type netDevRow struct {
	Name     string
	DevType  string
	Up       bool
	Wireless bool
	Bridge   bool
	Master   string // 所属网桥（非网桥接口）
	Ports    string // 网桥成员列表（仅网桥接口）
	MAC      string
	MTU      int
	IPs      string
	Link     string // 速率/双工/载波（非无线接口）
	Stats    string // 收/发 字节与包数
}

// deviceDetailPage 是设备详情页视图（字段映射见开发技术文档 §4.1）。
type deviceDetailPage struct {
	pageBase
	Device    *frpsclient.Device
	Host      string // 拼装访问地址 {session_id}.domain:port
	Serial    string // UBUS system.board 现拉（token 复用 /auth 登录凭据）；失败降级 device_id
	LanIP     string // UBUS luci-rpc getNetworkDevices 现拉：网桥（br-lan）首个 IPv4
	NetDevs   []netDevRow
	NetIfaces []netIfaceRow
	Snapshot  string // additional_data pretty json
	RPCError  string // 设备调用失败提示（不影响页面渲染）
}

// handleDeviceDetail 渲染设备详情页。
// 基础信息来自 frps_helper 北向；序列号走 UBUS system.board（除登录外不用 RPC），失败降级 device_id。
func (a *App) handleDeviceDetail(c *gin.Context) {
	deviceID := c.Param("id")
	dev, err := a.Frps.GetDevice(c.Request.Context(), deviceID)
	if err != nil {
		a.Log.Warn("device not found", "device_id", deviceID, "err", err)
		c.String(http.StatusNotFound, "设备未找到：%s", deviceID)
		return
	}

	host := a.Cfg.DeviceHost(dev.SessionID)
	serial := deviceID // 默认降级
	lanIP := ""
	rpcErr := ""

	// 仅在线设备尝试现拉（离线设备隧道不通，避免无效等待）。
	// 访问策略：除 /auth 登录外全部走 UBUS（用户实测确认 luci_token 可直接调 /ubus）。
	var netDevs []netDevRow
	var netIfaces []netIfaceRow
	if dev.SessionID != "" && dev.Status == "online" {
		var board map[string]any
		if uerr := a.Ubus.CallInto(c.Request.Context(), dev.SessionID, "system", "board", &board, map[string]any{}); uerr != nil {
			rpcErr = uerr.Error()
			a.Log.Warn("device detail: ubus system.board failed",
				"device_id", deviceID, "session_id", dev.SessionID, "err", uerr)
			board = nil
		}
		if s := pickStr(board, "board_id", "serial", "serial_number", "boardname", "board_name"); s != "" {
			serial = s
		}
		// 全量网卡（用户要求展示）：luci-rpc getNetworkDevices，
		// 同时取网桥首个 IPv4 作为内网管理 IP（原 todo8）
		nd, nerr := fetchNetDevs(c.Request.Context(), a, dev.SessionID)
		if nerr != nil {
			if rpcErr != "" {
				rpcErr += "；"
			}
			rpcErr += "网络接口: " + nerr.Error()
			a.Log.Warn("device detail: ubus getNetworkDevices failed",
				"device_id", deviceID, "session_id", dev.SessionID, "err", nerr)
		} else {
			netDevs = nd.rows
			lanIP = nd.lanIP
		}
		// 逻辑接口状态（lan/wan/loopback）：ubus network.interface dump
		ifs, ierr := fetchNetIfaces(c.Request.Context(), a, dev.SessionID)
		if ierr != nil {
			if rpcErr != "" {
				rpcErr += "；"
			}
			rpcErr += "逻辑接口: " + ierr.Error()
			a.Log.Warn("device detail: ubus network.interface dump failed",
				"device_id", deviceID, "session_id", dev.SessionID, "err", ierr)
		} else {
			netIfaces = ifs
		}
	} else if dev.Status != "online" {
		a.Log.Info("device detail: skip rpc for non-online device",
			"device_id", deviceID, "status", dev.Status)
	}

	u, _ := auth.CurrentUser(c)
	pg := deviceDetailPage{
		pageBase:  pageBase{ActiveMenu: "devices", CurrentUser: u, Config: a.Cfg},
		Device:    dev,
		Host:      host,
		Serial:    serial,
		LanIP:     lanIP,
		NetDevs:   netDevs,
		NetIfaces: netIfaces,
		Snapshot:  prettyJSON(dev.AdditionalData),
		RPCError:  rpcErr,
	}
	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/devices/detail", pg); rerr != nil {
		a.Log.Error("render device detail failed", "err", rerr)
	}
}

// prettyJSON 格式化 json.RawMessage 为缩进字符串；空/null 返回 "-"。
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "-"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// pickStr 从 map 中按 keys 顺序取第一个非空字符串值。
func pickStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// netDevsResult 是 getNetworkDevices 的解析产物：排序后的行视图 + 网桥管理 IP。
type netDevsResult struct {
	rows  []netDevRow
	lanIP string
}

// netDevRaw 对应 ubus luci-rpc getNetworkDevices 响应中单个网卡对象（实测结构见 UBUS访问指南.md §5.1）。
type netDevRaw struct {
	Name     string   `json:"name"`
	Wireless bool     `json:"wireless"`
	Up       bool     `json:"up"`
	MTU      int      `json:"mtu"`
	DevType  string   `json:"devtype"` // dsa/ethernet/wlan/bridge
	Master   string   `json:"master"`  // 所属网桥
	Bridge   bool     `json:"bridge"`
	Ports    []string `json:"ports"` // 网桥成员（仅网桥）
	IPAddrs  []struct {
		Address string `json:"address"`
		Netmask string `json:"netmask"`
	} `json:"ipaddrs"`
	MAC   string `json:"mac"`
	Stats struct {
		RxBytes   uint64 `json:"rx_bytes"`
		TxBytes   uint64 `json:"tx_bytes"`
		RxPackets uint64 `json:"rx_packets"`
		TxPackets uint64 `json:"tx_packets"`
	} `json:"stats"`
	Link struct {
		Speed   int    `json:"speed"` // -1 表示无效
		Duplex  string `json:"duplex"`
		Carrier bool   `json:"carrier"`
	} `json:"link"`
}

// fetchNetDevs 调用 ubus luci-rpc getNetworkDevices 拉取全量网卡并构建行视图。
// lanIP 取网桥（devtype=bridge）首个 IPv4，作为设备内网管理 IP。
func fetchNetDevs(ctx context.Context, a *App, sessionID string) (*netDevsResult, error) {
	var devs map[string]netDevRaw
	if err := a.Ubus.CallInto(ctx, sessionID, "luci-rpc", "getNetworkDevices", &devs, map[string]any{}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(devs))
	for name := range devs {
		names = append(names, name)
	}
	sort.Strings(names)

	res := &netDevsResult{rows: make([]netDevRow, 0, len(names))}
	for _, name := range names {
		d := devs[name]
		if d.Name == "" {
			d.Name = name
		}
		row := netDevRow{
			Name:     d.Name,
			DevType:  d.DevType,
			Up:       d.Up,
			Wireless: d.Wireless,
			Bridge:   d.Bridge,
			Master:   d.Master,
			MAC:      d.MAC,
			MTU:      d.MTU,
			Ports:    strings.Join(d.Ports, ", "),
		}
		ips := make([]string, 0, len(d.IPAddrs))
		for _, ip := range d.IPAddrs {
			ips = append(ips, ip.Address)
		}
		row.IPs = strings.Join(ips, ", ")
		if d.DevType != "wlan" && d.DevType != "bridge" {
			link := "carrier=" + boolStr(d.Link.Carrier)
			if d.Link.Speed > 0 {
				link = fmt.Sprintf("%dMb %s %s", d.Link.Speed, d.Link.Duplex, link)
			}
			row.Link = link
		}
		row.Stats = fmt.Sprintf("↓%s/%d包 ↑%s/%d包",
			fmtBytes(d.Stats.RxBytes), d.Stats.RxPackets,
			fmtBytes(d.Stats.TxBytes), d.Stats.TxPackets)
		res.rows = append(res.rows, row)
	}

	// 内网管理 IP：优先网桥（如 br-lan），兜底名称含 lan 的接口
	for _, name := range names {
		d := devs[name]
		if d.DevType == "bridge" && len(d.IPAddrs) > 0 && d.IPAddrs[0].Address != "" {
			res.lanIP = d.IPAddrs[0].Address
			break
		}
	}
	if res.lanIP == "" {
		for _, name := range names {
			d := devs[name]
			if strings.Contains(name, "lan") && len(d.IPAddrs) > 0 && d.IPAddrs[0].Address != "" {
				res.lanIP = d.IPAddrs[0].Address
				break
			}
		}
	}
	return res, nil
}

// boolStr 布尔转 yes/no。
func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// netIfaceRow 是逻辑接口表格的单行视图（来自 ubus network.interface dump）。
type netIfaceRow struct {
	Name   string
	Proto  string // static/dhcp/...
	Up     bool
	Device string // device（与 l3_device 不同时显示 "device → l3_device"）
	Uptime string
	IPv4   string // 192.168.100.1/24，多个逗号分隔
	DNS    string
	Routes string // 0.0.0.0/0 → 10.10.10.1
	Extra  string // data 摘要（dhcpserver/hostname/leasetime）
}

// netIfaceRaw 对应 ubus network.interface dump 响应中单个逻辑接口（实测结构见 UBUS访问指南.md §5.1）。
type netIfaceRaw struct {
	Interface string `json:"interface"`
	Up        bool   `json:"up"`
	Uptime    int64  `json:"uptime"`
	Proto     string `json:"proto"`
	Device    string `json:"device"`
	L3Device  string `json:"l3_device"`
	IPv4      []struct {
		Address string `json:"address"`
		Mask    int    `json:"mask"`
	} `json:"ipv4-address"`
	Route []struct {
		Target  string `json:"target"`
		Mask    int    `json:"mask"`
		Nexthop string `json:"nexthop"`
	} `json:"route"`
	DNSServer []string       `json:"dns-server"`
	Data      map[string]any `json:"data"`
}

// fetchNetIfaces 调用 ubus network.interface dump 拉取逻辑接口状态（lan/wan/loopback 等）。
// 响应数据在 result[1].interface 数组。
func fetchNetIfaces(ctx context.Context, a *App, sessionID string) ([]netIfaceRow, error) {
	var dump struct {
		Interface []netIfaceRaw `json:"interface"`
	}
	if err := a.Ubus.CallInto(ctx, sessionID, "network.interface", "dump", &dump, map[string]any{}); err != nil {
		return nil, err
	}
	rows := make([]netIfaceRow, 0, len(dump.Interface))
	for _, itf := range dump.Interface {
		row := netIfaceRow{
			Name:   itf.Interface,
			Proto:  itf.Proto,
			Up:     itf.Up,
			Uptime: fmtUptime(itf.Uptime),
		}
		if itf.Device != "" && itf.L3Device != "" && itf.Device != itf.L3Device {
			row.Device = itf.Device + " → " + itf.L3Device
		} else {
			row.Device = itf.Device
		}
		ips := make([]string, 0, len(itf.IPv4))
		for _, ip := range itf.IPv4 {
			ips = append(ips, fmt.Sprintf("%s/%d", ip.Address, ip.Mask))
		}
		row.IPv4 = strings.Join(ips, ", ")
		row.DNS = strings.Join(itf.DNSServer, ", ")
		routes := make([]string, 0, len(itf.Route))
		for _, r := range itf.Route {
			routes = append(routes, fmt.Sprintf("%s/%d → %s", r.Target, r.Mask, r.Nexthop))
		}
		row.Routes = strings.Join(routes, ", ")
		var extras []string
		for _, k := range []string{"dhcpserver", "hostname", "leasetime"} {
			if v, ok := itf.Data[k]; ok {
				extras = append(extras, fmt.Sprintf("%s=%v", k, v))
			}
		}
		row.Extra = strings.Join(extras, " ")
		rows = append(rows, row)
	}
	return rows, nil
}

// fmtUptime 秒数人性化显示（x天x小时 / x小时x分 / x分x秒 / x秒）。
func fmtUptime(sec int64) string {
	if sec < 0 {
		return "-"
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd%dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// fmtBytes 字节数人性化显示（B/KB/MB/GB/TB，保留 1 位小数）。
func fmtBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}
