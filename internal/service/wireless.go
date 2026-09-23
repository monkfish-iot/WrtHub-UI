package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"

	"wrthub-ui/internal/ubusrpc"
)

// Wireless 封装无线相关的设备访问调用（开发技术文档 §4.4 / §5.4）。
//
// 访问策略（用户明确要求）：除 /auth 登录外，所有数据调用一律走 UBUS JSON-RPC，
// 不再使用 LuCI RPC。token 复用 /auth 返回的 luci_token（由 ubusrpc 内部经
// lucirpc.Client 获取并缓存），调用形式：
//
//	URL:  POST /ubus
//	body: {"jsonrpc":"2.0","id":1,"method":"call","params":[<luci_token>,"<obj>","<method>",{...}]}
//
// 实测确认（OpenWrt 25.12.2 / NatShell WR3000k）：
//   - system.board ✅、uci.get ✅、luci-rpc.getNetworkDevices ✅、
//     iwinfo.info/assoclist/scan ✅（需设备安装 rpcd-mod-iwinfo 扩展）
//   - iwinfo.devices ✅（STA 模式设备实测返回 {"devices":["phy0-sta0"]}；
//     部分 AP 模式固件返回 -32002 Access denied，调用失败安全跳过）
//   - iwinfo ubus 对象的 device 参数用网络接口名（phy0-ap0/phy1-ap0/phy0-sta0）；
//     传不存在的接口名（如 wlan0）返回 result:[4] NOT_FOUND
//   - iwinfo.assoclist/scan 的数据在 result[1].results 数组中（parseObjectList 已兼容）
type Wireless struct {
	ubus *ubusrpc.Client
	log  *slog.Logger
}

// NewWireless 构造 Wireless 业务封装。log 用于打印降级/失败日志，nil 时用 slog.Default()。
func NewWireless(ubus *ubusrpc.Client, log *slog.Logger) *Wireless {
	if log == nil {
		log = slog.Default()
	}
	return &Wireless{ubus: ubus, log: log}
}

// WirelessDevices 调用 ubus luci-rpc getWirelessDevices 获取无线 radio/接口全景
// （用户指定：了解设备无线接口情况的**第一个查询**）。
// 响应为 map[radio名]radio 对象（radio0/radio1...），实测结构：
//   - radio.config：type/band/channel/htmode/path 等 radio 级配置
//   - radio.interfaces[]：section、config(network/mode/encryption/ssid)、ifname、
//     vlans、stations（实测为空数组，接入终端仍以 iwinfo.assoclist 为准）、iwinfo
//   - radio.iwinfo 与 interfaces[].iwinfo：信道/噪声/发射功率/频宽模式/BSSID/加密等
//
// interfaces[].ifname 即 iwinfo.* 系列调用的 device 参数（phy0-ap0/phy1-ap0）。
func (w *Wireless) WirelessDevices(ctx context.Context, sessionID string) (map[string]any, error) {
	var out map[string]any
	err := w.ubus.CallInto(ctx, sessionID, "luci-rpc", "getWirelessDevices", &out, map[string]any{})
	return out, err
}

// Interfaces 返回设备无线网络接口列表（如 phy0-ap0/phy1-ap0/phy0-sta0），
// 列表元素即为 info/assoclist/scan 的 device 参数。
//
// 首选 ubus luci-rpc getWirelessDevices {}（取各 radio.interfaces[].ifname），
// 合并 ubus iwinfo devices {}（含 STA 接口如 phy0-sta0，luci-rpc 可能不返回）；
// 两者均失败时兜底 ubus luci-rpc getNetworkDevices {} 过滤 wireless==true。
// 全部失败返回 nil。
func (w *Wireless) Interfaces(ctx context.Context, sessionID string) []string {
	set := make(map[string]struct{})

	// 1. luci-rpc getWirelessDevices（含 radio/SSID 配置，供 overview tab 使用）
	if wd, err := w.WirelessDevices(ctx, sessionID); err == nil && len(wd) > 0 {
		for _, radio := range wd {
			rm, ok := radio.(map[string]any)
			if !ok {
				continue
			}
			ifaces, ok := rm["interfaces"].([]any)
			if !ok {
				continue
			}
			for _, itf := range ifaces {
				im, ok := itf.(map[string]any)
				if !ok {
					continue
				}
				if name, _ := im["ifname"].(string); name != "" {
					set[name] = struct{}{}
				}
			}
		}
	}

	// 2. iwinfo devices（含 STA 接口如 phy0-sta0，合并补齐 luci-rpc 未返回的接口）
	var devList struct {
		Devices []string `json:"devices"`
	}
	if err := w.ubus.CallInto(ctx, sessionID, "iwinfo", "devices", &devList, map[string]any{}); err == nil {
		for _, d := range devList.Devices {
			set[d] = struct{}{}
		}
	}

	if len(set) > 0 {
		out := make([]string, 0, len(set))
		for name := range set {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}

	// 3. 兜底：getNetworkDevices 过滤 wireless==true
	var devs map[string]struct {
		Wireless bool `json:"wireless"`
	}
	if err := w.ubus.CallInto(ctx, sessionID, "luci-rpc", "getNetworkDevices", &devs, map[string]any{}); err == nil {
		out := make([]string, 0, len(devs))
		for name, d := range devs {
			if d.Wireless {
				out = append(out, name)
			}
		}
		if len(out) > 0 {
			sort.Strings(out)
			return out
		}
	}
	w.log.Warn("wireless: no wireless ifaces found", "session_id", sessionID)
	return nil
}

// Iwinfo 调用 ubus iwinfo info 拉取设备实时信息（信号/信道/BSSID/噪声等）。
// params: {"device":"phy0-ap0"}（网络接口名）。
func (w *Wireless) Iwinfo(ctx context.Context, sessionID, ifname string) (map[string]any, error) {
	var out map[string]any
	err := w.ubus.CallInto(ctx, sessionID, "iwinfo", "info", &out, map[string]any{"device": ifname})
	return out, err
}

// Stations 调用 ubus iwinfo assoclist 拉取接入终端列表（MAC/信号/速率/连接时间）。
// 聚合设备所有无线接口（如 phy0-ap0/phy1-ap0）的结果：终端可能连接在任一 radio 上，
// 仅查单接口会漏掉其他接口的终端（实测终端在 phy1-ap0，页面查 phy0-ap0 显示为空）。
// 每条记录附加 ifname 字段标识来源接口。
func (w *Wireless) Stations(ctx context.Context, sessionID string) ([]map[string]any, error) {
	ifaces := w.Interfaces(ctx, sessionID)
	if len(ifaces) == 0 {
		w.log.Warn("wireless: no wireless ifaces, stations empty", "session_id", sessionID)
		return nil, nil
	}
	var out []map[string]any
	var lastErr error
	for _, ifname := range ifaces {
		raw, err := w.ubus.Call(ctx, sessionID, "iwinfo", "assoclist", map[string]any{"device": ifname})
		if err != nil {
			lastErr = err
			w.log.Warn("wireless: assoclist failed", "session_id", sessionID, "ifname", ifname, "err", err)
			continue
		}
		for _, st := range parseObjectList(raw) {
			st["ifname"] = ifname
			out = append(out, st)
		}
	}
	// 所有接口都查询失败才报错；部分失败/全部为空按已获取数据返回
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// Scan 调用 ubus iwinfo scan 拉取周边 AP 扫描结果。
// params: {"device":"phy0-sta0"}；响应数据在 result[1].results 数组。
func (w *Wireless) Scan(ctx context.Context, sessionID, ifname string) ([]map[string]any, error) {
	raw, err := w.ubus.Call(ctx, sessionID, "iwinfo", "scan", map[string]any{"device": ifname})
	if err != nil {
		return nil, err
	}
	return parseObjectList(raw), nil
}

// parseObjectList 容错解析 ubus 返回的"对象列表"。
// 兼容三种固件形态：
//  1. []object 直接数组；
//  2. map 且其下 stations/results/items/assoclist/scanlist/devices/list 等键为数组
//     （如 rpcd-mod-iwinfo 返回 {"results":[...]}）；
//  3. map[X]object 形式：每对键值合并为一行，原 map 键记入 "key" 字段。
func parseObjectList(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// 1) 直接是数组
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	// 2) map 下已知键找数组
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	for _, key := range []string{"stations", "results", "items", "assoclist", "scanlist", "devices", "list"} {
		if v, ok := m[key]; ok {
			if err := json.Unmarshal(v, &arr); err == nil {
				return arr
			}
		}
	}
	// 3) map[X]object：合并每对为一行
	out := make([]map[string]any, 0, len(m))
	for k, v := range m {
		row := map[string]any{"key": k}
		var obj map[string]any
		if err := json.Unmarshal(v, &obj); err == nil {
			for kk, vv := range obj {
				row[kk] = vv
			}
		} else {
			var scalar any
			_ = json.Unmarshal(v, &scalar)
			row["value"] = scalar
		}
		out = append(out, row)
	}
	return out
}
