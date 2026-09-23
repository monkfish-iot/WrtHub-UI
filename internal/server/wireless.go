package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
)

// wirelessPage 是无线管理页视图（开发技术文档 §4.4 / §5.4）。
type wirelessPage struct {
	pageBase
	DeviceID        string
	DeviceName      string
	SessionID       string
	Status          string
	Interfaces      []string
	CurIfname       string
	Tab             string
	Iwinfo          map[string]any
	WirelessDevices map[string]any
	Stations        []map[string]any
	Scan            []map[string]any
	Error           string
}

// handleWireless 渲染设备的无线管理页。
// 仅在线设备尝试 RPC 现拉（离线设备隧道不通，避免无效等待）。
// tab=overview 显示接口实时信息；stations 显示接入终端；scan 显示周边 AP 扫描。
func (a *App) handleWireless(c *gin.Context) {
	deviceID := c.Param("id")
	dev, err := a.Frps.GetDevice(c.Request.Context(), deviceID)
	if err != nil {
		a.Log.Warn("device not found", "device_id", deviceID, "err", err)
		c.String(http.StatusNotFound, "设备未找到：%s", deviceID)
		return
	}

	tab := c.DefaultQuery("tab", "overview")
	ifaces := []string{"wlan0"}
	if dev.SessionID != "" && dev.Status == "online" {
		if got := a.Wireless.Interfaces(c.Request.Context(), dev.SessionID); len(got) > 0 {
			ifaces = got
		}
	}
	curIfname := c.DefaultQuery("ifname", "")
	// query 带来的旧 ifname（如历史 URL 里的 radio0）可能不在当前接口列表中，
	// 直接用会导致 iwinfo NOT_FOUND/空数据；不在列表则回退第一个真实接口。
	if curIfname == "" || !containsStr(ifaces, curIfname) {
		curIfname = ifaces[0]
	}

	u, _ := auth.CurrentUser(c)
	pg := wirelessPage{
		pageBase:   pageBase{ActiveMenu: "wireless", CurrentUser: u, Config: a.Cfg},
		DeviceID:   dev.DeviceID,
		DeviceName: dev.DeviceName,
		SessionID:  dev.SessionID,
		Status:     dev.Status,
		Interfaces: ifaces,
		CurIfname:  curIfname,
		Tab:        tab,
	}

	if dev.SessionID == "" || dev.Status != "online" {
		pg.Error = "设备离线，无法获取实时无线信息"
	} else {
		ctx := c.Request.Context()
		switch tab {
		case "stations":
			// 聚合所有无线接口的接入终端（终端可能在任一 radio 上，仅查单接口会漏）
			st, err := a.Wireless.Stations(ctx, dev.SessionID)
			if err != nil {
				pg.Error = err.Error()
			} else {
				pg.Stations = st
			}
		case "scan":
			sc, err := a.Wireless.Scan(ctx, dev.SessionID, curIfname)
			if err != nil {
				pg.Error = err.Error()
			} else {
				pg.Scan = formatScanResults(sc)
			}
		default: // overview
			info, err := a.Wireless.Iwinfo(ctx, dev.SessionID, curIfname)
			if err != nil {
				pg.Error = err.Error()
			} else {
				pg.Iwinfo = info
			}
			// 无线 radio/接口全景（getWirelessDevices，无线接口情况首选查询）；
			// 失败不阻塞页面（Interfaces() 内部已兜底 getNetworkDevices）
			if wd, werr := a.Wireless.WirelessDevices(ctx, dev.SessionID); werr == nil && len(wd) > 0 {
				pg.WirelessDevices = wd
			}
		}
	}

	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/wireless", pg); rerr != nil {
		a.Log.Error("render wireless page failed", "err", rerr)
	}
}

// containsStr 判断列表是否包含指定字符串。
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// formatScanResults 将 ubus iwinfo scan 原始结果格式化为模板友好的结构。
// 信号最强（dBm 值最大）的排在最前；每条记录包含：
//
//	ssid / bssid / band(2.4GHz|5GHz) / channel / mhz / signal(dBm)
//	quality_pct(0-100) / signal_class(success|warning|danger) / encryption(摘要)
func formatScanResults(raw []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		item := map[string]any{}

		ssid, _ := r["ssid"].(string)
		if ssid == "" {
			ssid = "(隐藏网络)"
		}
		item["ssid"] = ssid

		bssid, _ := r["bssid"].(string)
		item["bssid"] = bssid

		bandStr := "未知"
		if band, ok := r["band"].(float64); ok {
			switch int(band) {
			case 1:
				bandStr = "5GHz"
			case 2:
				bandStr = "2.4GHz"
			}
		}
		item["band"] = bandStr

		item["channel"] = r["channel"]
		item["mhz"] = r["mhz"]

		signal, _ := r["signal"].(float64)
		item["signal"] = signal

		quality, _ := r["quality"].(float64)
		qualityMax, _ := r["quality_max"].(float64)
		var qualityPct float64
		if qualityMax > 0 {
			qualityPct = quality / qualityMax * 100
		}
		item["quality_pct"] = qualityPct

		// 信号等级：>-50dBm 优秀 / -50~-67 良好 / <-67 较弱
		signalClass := "danger"
		if signal > -50 {
			signalClass = "success"
		} else if signal > -67 {
			signalClass = "warning"
		}
		item["signal_class"] = signalClass

		item["encryption"] = formatEncryption(r["encryption"])

		out = append(out, item)
	}

	// 按信号强度降序（dBm 值越大越强，-49 > -70）
	sort.SliceStable(out, func(i, j int) bool {
		si, _ := out[i]["signal"].(float64)
		sj, _ := out[j]["signal"].(float64)
		return si > sj
	})

	return out
}

// formatEncryption 将 iwinfo encryption 对象转为可读摘要。
// 输出示例：WPA2-PSK/CCMP、WPA/WPA2-PSK/TKIP/CCMP、开放
func formatEncryption(enc any) string {
	m, ok := enc.(map[string]any)
	if !ok {
		return "未知"
	}
	enabled, _ := m["enabled"].(bool)
	if !enabled {
		return "开放"
	}

	var wpaParts []string
	if wpa, ok := m["wpa"].([]any); ok {
		for _, w := range wpa {
			if n, ok := w.(float64); ok {
				switch int(n) {
				case 1:
					wpaParts = append(wpaParts, "WPA")
				case 2:
					wpaParts = append(wpaParts, "WPA2")
				case 3:
					wpaParts = append(wpaParts, "WPA3")
				}
			}
		}
	}
	if len(wpaParts) == 0 {
		wpaParts = []string{"WPA"}
	}

	var authStr string
	if auth, ok := m["authentication"].([]any); ok && len(auth) > 0 {
		if s, ok := auth[0].(string); ok {
			authStr = strings.ToUpper(s)
		}
	}

	var cipherStr string
	if ciphers, ok := m["ciphers"].([]any); ok && len(ciphers) > 0 {
		var cs []string
		for _, c := range ciphers {
			if s, ok := c.(string); ok {
				cs = append(cs, strings.ToUpper(s))
			}
		}
		if len(cs) > 0 {
			cipherStr = strings.Join(cs, "/")
		}
	}

	result := strings.Join(wpaParts, "/")
	if authStr != "" {
		result += "-" + authStr
	}
	if cipherStr != "" {
		result += "/" + cipherStr
	}
	return result
}
