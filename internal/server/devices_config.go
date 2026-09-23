package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/frpsclient"
	"wrthub-ui/internal/store"
)

// uciField 是 UCI 配置选项的键值对视图。
type uciField struct {
	Key   string
	Value string
}

// uciSection 是 UCI 配置的单个 section 视图（uci get 响应 values 的一个条目，
// 实测结构见 UBUS访问指南.md §5.1：.type/.name/.index/.anonymous 元字段 + 选项键值）。
type uciSection struct {
	Name      string // section 名（匿名 section 为 cfgXXXX）
	Type      string // .type（interface/wifi-device/zone/rule 等）
	Index     int    // .index 原始顺序
	Anonymous bool   // .anonymous
	Fields    []uciField
}

// ssidView 是无线 TAB 中单个 SSID（wifi-iface）的视图。
type ssidView struct {
	Section  string // section 名（如 default_radio0）
	SSID     string
	Mode     string // ap（本期固定）
	Enc      string // 原始 uci encryption 值（编辑表单回显；"" 等价 none）
	EncText  string // 加密显示名（开放/WPA2-PSK/...，未知值原样显示）
	Key      string // 当前密码（编辑表单回显，type=password 遮蔽显示）
	Network  string
	Hidden   bool
	Isolate  bool
	Disabled bool
	Ifname   string // 实时 ifname（getWirelessDevices interfaces[] 按 section 匹配）
}

// EncOptionsHTML 渲染加密模式下拉的 <option> 列表（§4.3，本期支持）。
// 与 ModeOptionsHTML 同理：Go 侧预拼 selected 返回 template.HTML，规避 html/template
// 转义器对属性位置条件输出的 ErrBadHTML 风险。Enc 为 ""（未设置）时选中 none。
func (s ssidView) EncOptionsHTML() template.HTML {
	var b strings.Builder
	for _, c := range encChoices {
		cur := s.Enc
		if cur == "" {
			cur = "none"
		}
		b.WriteString(`<option value="`)
		b.WriteString(html.EscapeString(c.Val))
		b.WriteString(`"`)
		if c.Val == cur {
			b.WriteString(` selected`)
		}
		b.WriteString(`>`)
		b.WriteString(html.EscapeString(c.Text))
		b.WriteString(`</option>`)
	}
	return template.HTML(b.String())
}

// encChoices 加密模式下拉选项（§4.3；encText 显示名与之一致）。
var encChoices = []struct{ Val, Text string }{
	{"none", "开放"},
	{"psk2", "WPA2-PSK"},
	{"psk-mixed", "WPA/WPA2 混合"},
	{"sae-mixed", "WPA2/WPA3 混合"},
	{"sae", "WPA3-SAE"},
}

// validEnc 加密值是否在支持列表内。
func validEnc(e string) bool {
	for _, c := range encChoices {
		if c.Val == e {
			return true
		}
	}
	return false
}

// isSaeFamily WPA3 系加密（sae/sae-mixed）：按 §3.4 抓包，LuCI 25.x 对该系自动补写 ocv=0。
func isSaeFamily(e string) bool { return e == "sae" || e == "sae-mixed" }

// encRequiresKey 该加密模式是否必须设置密码（§4.3：psk 类 8-63 字符）。
func encRequiresKey(e string) bool { return e != "none" && e != "" }

// radioView 是无线 TAB 中单个无线网卡（wifi-device）的视图。
type radioView struct {
	Section   string // radio0/radio1...
	Band      string // 2g/5g（uci）
	Country   string
	Channel   string
	Htmode    string // 原始 uci 值（如 HE80）
	Mode      string // 无线模式下拉当前值（n/ac/ax/be/NOHT，由 Htmode 推导）
	Width     string // 频宽下拉当前值（20/40/80/160，MHz，由 Htmode 推导）
	Txpower   string // 空 = 默认（最大）
	Disabled  bool
	Up        bool   // 实时状态（getWirelessDevices）
	HWName    string // 实时 hardware.name（如 MediaTek MT7981）
	HWModes   string // 实时 hwmodes_text（如 ax/b/g/n）
	Frequency int    // 实时 MHz
	Ifaces    []ssidView
	// 编辑表单下拉选项（P5：网卡参数编辑）
	CountryOpts []string
	ChannelOpts []string
	TxpowerOpts []string
	ModeOpts    string   // 无线模式下拉选项（JSON 数组字符串：["n","ac","ax"]）
	WidthsJSON  string   // 模式→频宽分组（JSON 对象字符串，供前端联动：{"n":["20","40"],...}）
	CurWidths   []string // 当前模式的频宽选项（初始渲染宽度下拉）
}

// BandText 频段显示名：uci band 优先，缺失时按实时频率推导。
func (r radioView) BandText() string {
	switch strings.ToLower(r.Band) {
	case "2g":
		return "2G"
	case "5g":
		return "5G"
	case "6g":
		return "6G"
	}
	if r.Frequency > 0 {
		if r.Frequency < 3000 {
			return "2G"
		}
		if r.Frequency < 6000 {
			return "5G"
		}
		return "6G"
	}
	return r.Band
}

// ModeOptionsHTML 渲染无线模式下拉的 <option> 列表（含"默认（未设置）"/"禁用HT"静态项，
// 动态项来自 ModeOpts JSON，由设备实时 htmodes 分组而来）。
// 在 Go 侧预拼 selected 并返回 template.HTML，而非在模板标签内用 {{if}} 输出条件属性：
// html/template 转义器对「range 管道迭代 + 属性位置 {{if}}」的组合会报
// ErrBadHTML（"\"" in attribute name），导致整页渲染失败（tplcheck 探针 e8/e10 对比定位）。
func (r radioView) ModeOptionsHTML() template.HTML {
	type modeOpt struct {
		val, text string
		sel       bool
	}
	opts := []modeOpt{
		{"", "默认（未设置）", r.Mode == ""},
		{"NOHT", "禁用HT", r.Mode == "NOHT"},
	}
	var ms []string
	if r.ModeOpts != "" {
		if err := json.Unmarshal([]byte(r.ModeOpts), &ms); err != nil {
			ms = nil
		}
	}
	for _, m := range ms {
		opts = append(opts, modeOpt{val: m, text: m, sel: m == r.Mode})
	}
	var b strings.Builder
	for _, o := range opts {
		b.WriteString(`<option value="`)
		b.WriteString(html.EscapeString(o.val))
		b.WriteString(`"`)
		if o.sel {
			b.WriteString(` selected`)
		}
		b.WriteString(`>`)
		b.WriteString(html.EscapeString(o.text))
		b.WriteString(`</option>`)
	}
	return template.HTML(b.String())
}

// wirelessView 是无线 TAB 视图（radio 卡片 + SSID 列表 + 实时状态回显；
// radio/SSID 编辑 API 见 handleDeviceConfigRadioEdit/handleDeviceConfigSSIDEdit，§5.3）。
type wirelessView struct {
	Radios []radioView
}

// EncOptionsHTML 新增 SSID 表单的加密下拉（零值 ssidView → Enc 为空，默认选中 none）。
func (w *wirelessView) EncOptionsHTML() template.HTML { return ssidView{}.EncOptionsHTML() }

// deviceConfigPage 是设备配置页视图（TAB：LAN / WAN(无则隐藏,只读) / 无线；
// 防火墙 TAB 暂时隐藏——用户确认，后续有需求再开发）。
type deviceConfigPage struct {
	pageBase
	Device   *frpsclient.Device
	LAN      []uciSection // LAN 组：lan/globals/br-lan device sections
	WAN      []uciSection // WAN 组；nil = 无 WAN（桥模式）→ WAN TAB 隐藏
	Wireless *wirelessView
	Error    string // 拉取失败提示（不影响页面渲染）
	FlashOK  string // 写操作成功提示（来自 ?ok= 查询参数）
	FlashErr string // 写操作失败提示（来自 ?err= 查询参数）
}

// handleDeviceConfig 渲染设备配置页。
// 数据走 UBUS uci.get（除 /auth 登录外全部走 UBUS）。无线网卡参数与 SSID 编辑/新增/删除均已支持
// （uci.set/add/delete + uci.apply rollback 降级链），LAN 编辑待实现（无线配置功能开发文档.md §7）。
// 防失联规则（§2.1.1，服务端强制）：WAN 永远只读且无写 API；无 WAN（桥模式）时 LAN 仅提示只读。
func (a *App) handleDeviceConfig(c *gin.Context) {
	deviceID := c.Param("id")
	dev, err := a.Frps.GetDevice(c.Request.Context(), deviceID)
	if err != nil {
		a.Log.Warn("device not found", "device_id", deviceID, "err", err)
		c.String(http.StatusNotFound, "设备未找到：%s", deviceID)
		return
	}

	u, _ := auth.CurrentUser(c)
	pg := deviceConfigPage{
		pageBase: pageBase{ActiveMenu: "devices", CurrentUser: u, Config: a.Cfg},
		Device:   dev,
		FlashOK:  c.Query("ok"),
		FlashErr: c.Query("err"),
	}

	var errs []string
	if dev.SessionID == "" || dev.Status != "online" {
		errs = append(errs, "设备离线，无法获取配置")
		a.Log.Info("device config: skip ubus for non-online device", "device_id", deviceID, "status", dev.Status)
	} else {
		ctx := c.Request.Context()
		// network 配置 → LAN/WAN 分组（WAN 不存在 = 桥模式）
		secs, nerr := fetchUciSections(ctx, a, dev.SessionID, "network")
		if nerr != nil {
			errs = append(errs, "网络配置: "+nerr.Error())
			a.Log.Warn("device config: uci.get network failed",
				"device_id", deviceID, "session_id", dev.SessionID, "err", nerr)
		} else {
			pg.LAN, pg.WAN = splitLanWan(secs)
		}
		// wireless 配置 + getWirelessDevices 实时状态
		wv, werr := buildWirelessView(ctx, a, dev.SessionID)
		if werr != nil {
			errs = append(errs, "无线配置: "+werr.Error())
			a.Log.Warn("device config: wireless build failed",
				"device_id", deviceID, "session_id", dev.SessionID, "err", werr)
		} else {
			pg.Wireless = wv
		}
	}
	if len(errs) > 0 {
		pg.Error = strings.Join(errs, "；")
	}
	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/devices/config", pg); rerr != nil {
		a.Log.Error("render device config failed", "err", rerr)
	}
}

// fetchUciSections 调用 ubus uci get 拉取整个配置文件（省略 section 返回全部），
// 解析 values 为按 .index 排序的 section 列表，选项按键名排序。
func fetchUciSections(ctx context.Context, a *App, sessionID, config string) ([]uciSection, error) {
	var resp struct {
		Values map[string]map[string]any `json:"values"`
	}
	if err := a.Ubus.CallInto(ctx, sessionID, "uci", "get", &resp, map[string]any{"config": config}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(resp.Values))
	for name := range resp.Values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]uciSection, 0, len(names))
	for _, name := range names {
		sm := resp.Values[name]
		sec := uciSection{Name: name}
		if t, ok := sm[".type"].(string); ok {
			sec.Type = t
		}
		if b, ok := sm[".anonymous"].(bool); ok {
			sec.Anonymous = b
		}
		if idx, ok := sm[".index"].(float64); ok {
			sec.Index = int(idx)
		}
		keys := make([]string, 0, len(sm))
		for k := range sm {
			if strings.HasPrefix(k, ".") { // 跳过 .type/.name/.index/.anonymous 元字段
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sec.Fields = append(sec.Fields, uciField{Key: k, Value: fmtUciValue(sm[k])})
		}
		out = append(out, sec)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Index < out[j].Index
	})
	return out, nil
}

// splitLanWan 将 network 配置的 sections 按 LAN/WAN 语义分组（不硬编码匿名 section 名）：
// LAN = lan + globals + br-lan device section；WAN = wan。其余（loopback/无关 device）不展示。
func splitLanWan(sections []uciSection) (lan, wan []uciSection) {
	for _, s := range sections {
		switch {
		case s.Name == "lan" || s.Name == "globals":
			lan = append(lan, s)
		case s.Name == "wan":
			wan = append(wan, s)
		case s.Type == "device" && secField(s, "name") == "br-lan":
			lan = append(lan, s)
		}
	}
	return lan, wan
}

// buildWirelessView 构建无线 TAB 视图：uci.get wireless（radio/SSID 配置）+
// luci-rpc getWirelessDevices（实时状态/实时 ifname）。实时状态拉取失败仅降级显示（不报错）。
// wifi-iface 通过 device 选项归属到 radio；实时 ifname 通过 interfaces[].section == wifi-iface section 名匹配。
func buildWirelessView(ctx context.Context, a *App, sessionID string) (*wirelessView, error) {
	sections, err := fetchUciSections(ctx, a, sessionID, "wireless")
	if err != nil {
		return nil, err
	}
	wv := &wirelessView{}
	live, lerr := a.Wireless.WirelessDevices(ctx, sessionID)
	if lerr != nil {
		a.Log.Warn("device config: getWirelessDevices failed (live status degraded)",
			"session_id", sessionID, "err", lerr)
		live = nil
	}

	radioIdx := make(map[string]int)
	for _, s := range sections {
		if s.Type != "wifi-device" {
			continue
		}
		rv := radioView{
			Section:  s.Name,
			Band:     secField(s, "band"),
			Country:  secField(s, "country"),
			Channel:  secField(s, "channel"),
			Htmode:   secField(s, "htmode"),
			Txpower:  secField(s, "txpower"),
			Disabled: uciTruthy(secField(s, "disabled")),
		}
		var liveHt []string
		if lm, ok := live[s.Name].(map[string]any); ok {
			rv.Up = liveBool(lm, "up")
			if iw, ok := lm["iwinfo"].(map[string]any); ok {
				rv.HWModes = liveStr(iw, "hwmodes_text")
				if f, ok := iw["frequency"].(float64); ok {
					rv.Frequency = int(f)
				}
				if hw, ok := iw["hardware"].(map[string]any); ok {
					rv.HWName = liveStr(hw, "name")
				}
				if hm, ok := iw["htmodes"].([]any); ok {
					for _, m := range hm {
						if ms, ok := m.(string); ok {
							liveHt = append(liveHt, ms)
						}
					}
				}
			}
		}
		rv.CountryOpts = countryOptions(rv.Country)
		rv.ChannelOpts = channelOptions(rv.Band, rv.Channel)
		rv.TxpowerOpts = txpowerOptions()
		// 无线模式/频宽联动数据：按设备实时 htmodes 分组（HT*→n、VHT*→ac、HE*→ax、EHT*→be）
		groups := htGroups(htmodeOptions(rv.Htmode, liveHt))
		rv.Mode, rv.Width = splitHtmode(rv.Htmode)
		if ms, err := json.Marshal(modeKeys(groups)); err == nil {
			rv.ModeOpts = string(ms)
		}
		if ws, err := json.Marshal(groups); err == nil {
			rv.WidthsJSON = string(ws)
		}
		if ws, ok := groups[rv.Mode]; ok && len(ws) > 0 {
			if rv.Width == "" || !containsStr(ws, rv.Width) {
				rv.Width = ws[0]
			}
			rv.CurWidths = ws
		}
		radioIdx[s.Name] = len(wv.Radios)
		wv.Radios = append(wv.Radios, rv)
	}

	for _, s := range sections {
		if s.Type != "wifi-iface" {
			continue
		}
		sv := ssidView{
			Section:  s.Name,
			SSID:     secField(s, "ssid"),
			Mode:     secField(s, "mode"),
			Enc:      secField(s, "encryption"),
			EncText:  encText(secField(s, "encryption")),
			Key:      secField(s, "key"),
			Network:  secField(s, "network"),
			Hidden:   uciTruthy(secField(s, "hidden")),
			Isolate:  uciTruthy(secField(s, "isolate")),
			Disabled: uciTruthy(secField(s, "disabled")),
		}
		devName := secField(s, "device")
		if li, ok := live[devName].(map[string]any); ok {
			if ifaces, ok := li["interfaces"].([]any); ok {
				for _, itf := range ifaces {
					im, ok := itf.(map[string]any)
					if !ok || im["section"] != s.Name {
						continue
					}
					sv.Ifname, _ = im["ifname"].(string)
					break
				}
			}
		}
		if ri, ok := radioIdx[devName]; ok {
			wv.Radios[ri].Ifaces = append(wv.Radios[ri].Ifaces, sv)
		}
	}
	return wv, nil
}

// secField 读取 section 选项值。
func secField(s uciSection, key string) string {
	for _, f := range s.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

// uciTruthy UCI 选项真值判断（uci 值均为字符串）。
func uciTruthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

// liveStr 读取实时状态 map 的字符串字段。
func liveStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// liveBool 读取实时状态 map 的布尔字段。
func liveBool(m map[string]any, key string) bool {
	b, ok := m[key].(bool)
	return ok && b
}

// encText 加密模式显示名（§4.3 下拉对应表；未知值原样返回）。
func encText(e string) string {
	switch e {
	case "", "none":
		return "开放"
	case "psk2":
		return "WPA2-PSK"
	case "psk-mixed":
		return "WPA/WPA2 混合"
	case "sae-mixed":
		return "WPA2/WPA3 混合"
	case "sae":
		return "WPA3-SAE"
	default:
		return e
	}
}

// countryOptions 国家（regdomain）下拉选项：常用列表（ISO 3166 alpha-2 + 00 世界区），
// 当前值不在列表中时补入（§8.3 待确认项：先用常用列表，实测后按需扩充）。
func countryOptions(cur string) []string {
	base := []string{
		"00", "CN", "US", "DE", "GB", "FR", "IT", "ES", "NL", "SE",
		"JP", "KR", "IN", "AU", "CA", "BR", "RU", "SG", "HK", "TW",
	}
	if cur != "" && !containsStr(base, cur) {
		base = append([]string{cur}, base...)
	}
	return base
}

// channelOptions 信道下拉选项：2G auto/1-13；5G auto/36-64/100-144/149-165（§4.1）。
// band 未知时按 2G 处理；当前值不在列表中时补入（如 6G 设备）。
func channelOptions(band, cur string) []string {
	var out []string
	if strings.EqualFold(band, "5g") {
		out = []string{"auto", "36", "40", "44", "48", "52", "56", "60", "64",
			"100", "104", "108", "112", "116", "120", "124", "128", "132", "136", "140", "144",
			"149", "153", "157", "161", "165"}
	} else {
		out = []string{"auto"}
		for i := 1; i <= 13; i++ {
			out = append(out, strconv.Itoa(i))
		}
	}
	if cur != "" && !containsStr(out, cur) {
		out = append([]string{cur}, out...)
	}
	return out
}

// htmodeOptions 频宽下拉选项：优先取实时 htmodes（设备实际支持），兜底静态常见列表；
// 当前值不在列表中时补入。
func htmodeOptions(cur string, live []string) []string {
	out := live
	if len(out) == 0 {
		out = []string{"HT20", "HT40", "VHT20", "VHT40", "VHT80", "VHT160", "HE20", "HE40", "HE80", "HE160"}
	}
	if cur != "" && !containsStr(out, cur) {
		out = append([]string{cur}, out...)
	}
	return out
}

// txpowerOptions 发射功率下拉选项："" = 默认（最大，提交时删除 txpower 选项），0-23 dBm（§8.4 待实测上限）。
func txpowerOptions() []string {
	out := []string{""}
	for i := 0; i <= 23; i++ {
		out = append(out, strconv.Itoa(i))
	}
	return out
}

// htModePrefix 无线模式 → htmode 前缀映射（ax+80 → HE80）。
var htModePrefix = map[string]string{"n": "HT", "ac": "VHT", "ax": "HE", "be": "EHT"}

// splitHtmode 拆分 htmode 为（无线模式, 频宽MHz）：HE80→(ax,80)、HT40-→(n,40)、NOHT→(NOHT,"")、""→("","")。
func splitHtmode(ht string) (mode, width string) {
	if ht == "" {
		return "", ""
	}
	i := 0
	for i < len(ht) && (ht[i] < '0' || ht[i] > '9') {
		i++
	}
	prefix := ht[:i]
	w := strings.TrimSuffix(strings.TrimSuffix(ht[i:], "-"), "+")
	switch prefix {
	case "HT":
		return "n", w
	case "VHT":
		return "ac", w
	case "HE":
		return "ax", w
	case "EHT":
		return "be", w
	case "NOHT":
		return "NOHT", ""
	default:
		return "", w
	}
}

// htGroups 把 htmodes 列表按无线模式分组：["HT20","VHT80","HE40"] → {"n":["20"],"ac":["80"],"ax":["40"]}（宽度升序）。
func htGroups(opts []string) map[string][]string {
	g := map[string][]string{}
	for _, o := range opts {
		m, w := splitHtmode(o)
		if m == "" || w == "" {
			continue
		}
		if !containsStr(g[m], w) {
			g[m] = append(g[m], w)
		}
	}
	for m := range g {
		ws := g[m]
		sort.Slice(ws, func(i, j int) bool {
			a, _ := strconv.Atoi(ws[i])
			b, _ := strconv.Atoi(ws[j])
			return a < b
		})
		g[m] = ws
	}
	return g
}

// modeKeys 模式下拉选项，按协议时间序排列（n/ac/ax/be）。
func modeKeys(g map[string][]string) []string {
	out := []string{}
	for _, m := range []string{"n", "ac", "ax", "be"} {
		if _, ok := g[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

// fmtUciValue 格式化 UCI 选项值：字符串原样、布尔 yes/no、数字去尾零、数组逗号拼接。
func fmtUciValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return boolStr(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, fmtUciValue(e))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprintf("%v", t)
	}
}

// handleDeviceConfigRadioEdit 提交无线网卡参数修改（无线配置功能开发文档.md §5.3 radio API，P5）。
// 流程（与 openwrt.log 抓包中 LuCI 流程一致，全部走 UBUS）：
//  1. uci.set  暂存修改（LuCI 同款调用，实测可用）；
//  2. uci.changes 仅日志（LuCI 用它校验暂存）；
//  3. uci.apply {"rollback":true,"timeout":30} 提交并触发 reload——失败自动回滚（防失联）；
//     成功后立即 uci.confirm 确认（rollback 窗口内确认，配置持久化）；
//  4. uci.apply 被 ACL 拒绝等失败 → 降级 uci.commit（写入但不 reload），显式警告需 wifi reload。
//
// 仅修改与当前配置不同的选项（幂等）；txpower 选“默认”时删除该选项（uci.delete）。
// 所有结果记审计日志；成功/失败通过 ?ok=/?err= 重定向回显。
func (a *App) handleDeviceConfigRadioEdit(c *gin.Context) {
	deviceID := c.Param("id")
	dev, err := a.Frps.GetDevice(c.Request.Context(), deviceID)
	if err != nil {
		c.String(http.StatusNotFound, "设备未找到：%s", deviceID)
		return
	}
	back := "/devices/" + deviceID + "/config?tab=wireless"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&err="+url.QueryEscape(msg)) }

	u, _ := auth.CurrentUser(c)
	audit := func(ok bool, detail map[string]any) {
		action := "wireless.radio_edit_failed"
		if ok {
			action = "wireless.radio_edit"
		}
		_ = store.RecordAudit(a.DB, store.AuditEntry{
			UserID: u.ID, Username: u.Username, Action: action,
			Target: deviceID, Detail: detail,
			IP: c.ClientIP(), UserAgent: c.Request.UserAgent(),
		})
	}

	radio := c.PostForm("radio")
	if radio == "" {
		errMsg("缺少 radio 参数")
		return
	}
	if dev.SessionID == "" || dev.Status != "online" {
		errMsg("设备离线，无法修改配置")
		return
	}
	ctx := c.Request.Context()

	// 校验 section 存在且为 wifi-device（不信任前端提交的 radio 名）
	secs, err := fetchUciSections(ctx, a, dev.SessionID, "wireless")
	if err != nil {
		audit(false, map[string]any{"radio": radio, "step": "uci.get wireless", "err": err.Error()})
		errMsg("读取无线配置失败: " + err.Error())
		return
	}
	var cur *uciSection
	for i := range secs {
		if secs[i].Name == radio && secs[i].Type == "wifi-device" {
			cur = &secs[i]
			break
		}
	}
	if cur == nil {
		audit(false, map[string]any{"radio": radio, "step": "validate", "err": "unknown section"})
		errMsg("未知的无线网卡：" + radio)
		return
	}

	// 组装修改集：仅写入与当前值不同的选项；txpower 空 = 默认（删除选项）
	values := map[string]any{}
	var delOpts []string
	curDisabled := "0"
	if uciTruthy(secField(*cur, "disabled")) {
		curDisabled = "1"
	}
	setIf := func(key, newV, oldV string) {
		if newV != oldV {
			values[key] = newV
		}
	}
	setIf("country", c.PostForm("country"), secField(*cur, "country"))
	setIf("channel", c.PostForm("channel"), secField(*cur, "channel"))
	// 无线模式+频宽联动下拉 → 合成 htmode（ax+80 → HE80）；
	// 模式选"默认（未设置）" → 删除 htmode 选项；NOHT → 禁用 HT
	switch mode := c.PostForm("mode"); mode {
	case "":
		if secField(*cur, "htmode") != "" {
			delOpts = append(delOpts, "htmode")
		}
	case "NOHT":
		setIf("htmode", "NOHT", secField(*cur, "htmode"))
	default:
		width := strings.TrimSpace(c.PostForm("width"))
		if width == "" {
			// 频宽未选择（前端禁用或数据缺失）：保持现有 htmode 不变
			break
		}
		prefix, ok := htModePrefix[mode]
		if !ok {
			audit(false, map[string]any{"radio": radio, "step": "validate", "err": "invalid mode " + mode})
			errMsg("无效的无线模式：" + mode)
			return
		}
		setIf("htmode", prefix+width, secField(*cur, "htmode"))
	}
	if txpower := strings.TrimSpace(c.PostForm("txpower")); txpower == "" {
		if secField(*cur, "txpower") != "" {
			delOpts = append(delOpts, "txpower")
		}
	} else {
		setIf("txpower", txpower, secField(*cur, "txpower"))
	}
	disabled := "0"
	if c.PostForm("disabled") == "1" {
		disabled = "1"
	}
	setIf("disabled", disabled, curDisabled)

	if len(values) == 0 && len(delOpts) == 0 {
		okMsg("参数未变化，无需修改")
		return
	}

	// 1) uci.set 暂存（CallWriteInto：写方法空响应即成功，不做空数据重试）
	if len(values) > 0 {
		if err := a.Ubus.CallWriteInto(ctx, dev.SessionID, "uci", "set", &map[string]any{},
			map[string]any{"config": "wireless", "section": radio, "values": values}); err != nil {
			audit(false, map[string]any{"radio": radio, "step": "uci.set", "values": values, "err": err.Error()})
			errMsg("uci.set 失败: " + err.Error())
			return
		}
	}
	// 2) uci.delete（如 txpower 复位）
	for _, opt := range delOpts {
		if err := a.Ubus.CallWriteInto(ctx, dev.SessionID, "uci", "delete", &map[string]any{},
			map[string]any{"config": "wireless", "section": radio, "option": opt}); err != nil {
			audit(false, map[string]any{"radio": radio, "step": "uci.delete", "option": opt, "err": err.Error()})
			errMsg("uci.delete " + opt + " 失败: " + err.Error())
			return
		}
	}

	// 3) 暂存检查（仅日志，与 LuCI 行为一致）
	var changes any
	if err := a.Ubus.CallInto(ctx, dev.SessionID, "uci", "changes", &changes, map[string]any{}); err != nil {
		a.Log.Warn("radio edit: uci.changes failed", "device_id", deviceID, "err", err)
	} else {
		a.Log.Info("radio edit: staged changes", "device_id", deviceID, "radio", radio, "changes", changes)
	}

	// 4) 生效降级链（与 SSID 编辑共用，见 finalizeWirelessApply）：
	//    ① uci.apply {rollback:true,timeout:30} + confirm；② 降级 uci.commit + network.reload；③ 仍失败报告暂存状态。
	path, msg, detail, ok := a.finalizeWirelessApply(ctx, dev.SessionID, deviceID)
	auditDetail := map[string]any{"radio": radio, "values": values, "deleted": delOpts, "applied": path}
	if detail != "" {
		auditDetail["detail"] = detail
	}
	audit(ok, auditDetail)
	if ok {
		okMsg(radio + "：" + msg)
	} else {
		errMsg(msg)
	}
}

// finalizeWirelessApply wireless 修改后的生效降级链（无线配置功能开发文档.md §3.3，radio/SSID 编辑共用）。
// 流程：
//  1. uci.apply {"rollback":true,"timeout":30} 提交并触发 reload（失败自动回滚，防失联），成功后立即
//     uci.confirm 在 rollback 窗口内确认（否则超时自动还原）；
//  2. apply 被 ACL 拒绝等失败 → 降级 uci.commit wireless（持久化）+ network.reload（触发 netifd 重载）；
//  3. commit 也被拒：修改仅剩暂存（/tmp/.uci，重启丢失、运行中不变），按暂存状态给出准确提示。
//
// 返回：path 生效路径（审计用）、msg 用户提示、detail 审计附加信息（如暂存 changes）；
// ok 为 true 时调用方走成功提示（okMsg），false 走错误提示（errMsg）。
// 注意：uci.apply 会提交所有 config 的暂存；uci.commit 仅 wireless。
func (a *App) finalizeWirelessApply(ctx context.Context, sessionID, deviceID string) (path, msg, detail string, ok bool) {
	var applyResp map[string]any
	applyErr := a.Ubus.CallWriteInto(ctx, sessionID, "uci", "apply", &applyResp,
		map[string]any{"rollback": true, "timeout": 30})
	if applyErr == nil {
		a.Log.Info("wireless apply: uci.apply ok", "device_id", deviceID, "resp", applyResp)
		if cerr := a.Ubus.CallWriteInto(ctx, sessionID, "uci", "confirm", &map[string]any{}, map[string]any{}); cerr != nil {
			a.Log.Warn("wireless apply: uci.confirm failed (rollback may revert)", "device_id", deviceID, "err", cerr)
			return "apply+confirm_failed",
				"配置已应用，但 confirm 失败（" + cerr.Error() + "），回滚超时后设备可能自动还原",
				cerr.Error(), true
		}
		return "apply+confirm", "配置已应用生效（uci.apply + rollback 确认）", "", true
	}
	a.Log.Warn("wireless apply: uci.apply denied/failed, fallback to commit+reload",
		"device_id", deviceID, "err", applyErr)

	// 降级①：uci.commit wireless（持久化到 /etc/config/wireless）
	if commitErr := a.Ubus.CallWriteInto(ctx, sessionID, "uci", "commit", &map[string]any{},
		map[string]any{"config": "wireless"}); commitErr != nil {
		// 降级链全断：查询暂存状态帮助排查（暂存≠生效，重启丢失）
		var changes any
		chgStr := "查询失败"
		stagedEmpty := false
		if err := a.Ubus.CallInto(ctx, sessionID, "uci", "changes", &changes, map[string]any{}); err == nil {
			if b, merr := json.Marshal(changes); merr == nil {
				chgStr = string(b)
			}
			if m, ok2 := changes.(map[string]any); ok2 {
				if inner, ok3 := m["changes"].(map[string]any); ok3 {
					stagedEmpty = len(inner) == 0
				} else if len(m) == 0 {
					stagedEmpty = true
				}
			}
		}
		a.Log.Warn("wireless apply: uci.commit failed, staged changes", "device_id", deviceID, "changes", chgStr)
		d := fmt.Sprintf("apply_err=%s commit_err=%s staged_changes=%s", applyErr, commitErr, chgStr)
		if stagedEmpty {
			return "staged",
				"uci.apply 失败（" + applyErr.Error() + "），uci.commit 失败（" + commitErr.Error() +
					"）。但设备暂存为空（changes={}）——修改可能已直接生效，请刷新页面核实无线参数；若未生效请在设备 ACL（/usr/share/rpcd/acl.d/）放行 uci 的 commit/apply 后重试",
				d, false
		}
		return "staged",
			"uci.apply 失败（" + applyErr.Error() + "），uci.commit 失败（" + commitErr.Error() +
				"）。修改仍为暂存状态（changes=" + chgStr + "），未持久化也未生效；请在设备 ACL（/usr/share/rpcd/acl.d/）放行 uci 的 commit/apply 后重试",
			d, false
	}

	// 降级②：network.reload 触发 netifd 重载 wireless 配置（等效 apply 生效，无 rollback 保护）
	if reloadErr := a.Ubus.CallWriteInto(ctx, sessionID, "network", "reload", &map[string]any{}, map[string]any{}); reloadErr != nil {
		a.Log.Warn("wireless apply: network.reload failed", "device_id", deviceID, "err", reloadErr)
		return "commit_no_reload",
			"配置已持久化（uci.commit），但 network.reload 失败（" + reloadErr.Error() +
				"）：当前运行的无线未重载，重启设备后生效；或在设备上手工执行 wifi reload",
			reloadErr.Error(), true
	}
	return "commit+reload", "配置已提交并通过 network.reload 生效", "", true
}

// handleDeviceConfigSSIDEdit 新增/修改 SSID（无线配置功能开发文档.md §5.3 ssid API，P5；
// 原始报文参考 §3.4 openwrt2.log 抓包）。section 为空 = 新增（uci.add，命名 section 便于管理），
// 否则为编辑（uci.set 仅写变化项）。
// 校验（§6 服务端强制）：ssid 1-32 字节；加密 ∈ §4.3 枚举；psk 类加密密码 8-63 字符；
// 编辑时校验 section 存在且为 wifi-iface 且归属所提交 radio。
// 抓包要点：sae 系加密自动补写 ocv="0"（LuCI 25.x 行为，兼容旧客户端）；
// 切到开放/非 sae 系时删除 key/ocv 选项。生效走 finalizeWirelessApply 降级链。
func (a *App) handleDeviceConfigSSIDEdit(c *gin.Context) {
	deviceID := c.Param("id")
	dev, err := a.Frps.GetDevice(c.Request.Context(), deviceID)
	if err != nil {
		c.String(http.StatusNotFound, "设备未找到：%s", deviceID)
		return
	}
	back := "/devices/" + deviceID + "/config?tab=wireless"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&err="+url.QueryEscape(msg)) }

	u, _ := auth.CurrentUser(c)
	audit := func(ok bool, detail map[string]any) {
		action := "wireless.ssid_edit_failed"
		if ok {
			action = "wireless.ssid_edit"
		}
		_ = store.RecordAudit(a.DB, store.AuditEntry{
			UserID: u.ID, Username: u.Username, Action: action,
			Target: deviceID, Detail: detail,
			IP: c.ClientIP(), UserAgent: c.Request.UserAgent(),
		})
	}

	radio := c.PostForm("radio")
	section := strings.TrimSpace(c.PostForm("section")) // 空 = 新增
	ssid := strings.TrimSpace(c.PostForm("ssid"))
	enc := c.PostForm("encryption")
	key := c.PostForm("key")
	network := strings.TrimSpace(c.PostForm("network"))
	flag := func(name string) string {
		if c.PostForm(name) == "1" {
			return "1"
		}
		return "0"
	}
	hidden, isolate, disabled := flag("hidden"), flag("isolate"), flag("disabled")

	isAdd := section == ""

	if radio == "" || ssid == "" {
		errMsg("缺少 radio 或 ssid 参数")
		return
	}
	if len(ssid) > 32 {
		errMsg(fmt.Sprintf("SSID 过长：%d 字节（上限 32 字节，中文每字 3 字节）", len(ssid)))
		return
	}
	if !validEnc(enc) {
		errMsg("不支持的加密模式：" + enc)
		return
	}
	if encRequiresKey(enc) && (len(key) < 8 || len(key) > 63) {
		errMsg("该加密模式需要 8-63 位密码")
		return
	}
	if dev.SessionID == "" || dev.Status != "online" {
		errMsg("设备离线，无法修改配置")
		return
	}
	ctx := c.Request.Context()

	// 读取 wireless 配置做服务端校验（不信任前端提交的 section/radio 名）
	secs, err := fetchUciSections(ctx, a, dev.SessionID, "wireless")
	if err != nil {
		audit(false, map[string]any{"section": section, "radio": radio, "step": "uci.get wireless", "err": err.Error()})
		errMsg("读取无线配置失败: " + err.Error())
		return
	}
	existing := map[string]bool{}
	var cur *uciSection
	var radioSec *uciSection
	for i := range secs {
		existing[secs[i].Name] = true
		switch {
		case secs[i].Name == radio && secs[i].Type == "wifi-device":
			radioSec = &secs[i]
		case secs[i].Name == section && secs[i].Type == "wifi-iface":
			cur = &secs[i]
		}
	}
	if radioSec == nil {
		audit(false, map[string]any{"section": section, "radio": radio, "step": "validate", "err": "unknown radio"})
		errMsg("未知的无线网卡：" + radio)
		return
	}
	if !isAdd {
		if cur == nil {
			audit(false, map[string]any{"section": section, "radio": radio, "step": "validate", "err": "unknown section"})
			errMsg("未知的 SSID section：" + section)
			return
		}
		if devName := secField(*cur, "device"); devName != radio {
			audit(false, map[string]any{"section": section, "radio": radio, "step": "validate", "err": "section belongs to " + devName})
			errMsg("SSID " + section + " 不属于网卡 " + radio + "（实际属于 " + devName + "）")
			return
		}
	}

	var addName string
	values := map[string]any{}
	var delOpts []string

	if isAdd {
		// 新增：生成唯一命名 section（wrthub_<radio>、wrthub_<radio>_2…），全量组装 values
		addName = "wrthub_" + radio
		for n := 2; existing[addName]; n++ {
			addName = fmt.Sprintf("wrthub_%s_%d", radio, n)
		}
		values["device"] = radio
		values["mode"] = "ap" // 本期固定 ap（§4.2）
		values["ssid"] = ssid
		values["encryption"] = enc
		if encRequiresKey(enc) {
			values["key"] = key
		}
		if network == "" {
			network = "lan" // 默认 lan（§4.2）
		}
		values["network"] = network
		if hidden == "1" {
			values["hidden"] = "1"
		}
		if isolate == "1" {
			values["isolate"] = "1"
		}
		if disabled == "1" {
			values["disabled"] = "1"
		}
		if isSaeFamily(enc) {
			values["ocv"] = "0" // §3.4：LuCI 25.x 对 sae 系自动补写，兼容旧客户端
		}
	} else {
		// 编辑：仅写与当前值不同的选项（幂等）；比较时把未设置（""）的加密视为 none
		setIf := func(k, newV, oldV string) {
			if newV != oldV {
				values[k] = newV
			}
		}
		oldEnc := secField(*cur, "encryption")
		if oldEnc == "" {
			oldEnc = "none"
		}
		setIf("ssid", ssid, secField(*cur, "ssid"))
		setIf("encryption", enc, oldEnc)
		oldKey := secField(*cur, "key")
		oldOCV := secField(*cur, "ocv")
		if enc == "none" {
			// 切到开放：清理 key；从 sae 系切出时清理 ocv
			if oldKey != "" {
				delOpts = append(delOpts, "key")
			}
			if isSaeFamily(oldEnc) && oldOCV != "" {
				delOpts = append(delOpts, "ocv")
			}
		} else {
			setIf("key", key, oldKey)
			if isSaeFamily(enc) {
				if oldOCV != "0" {
					values["ocv"] = "0" // §3.4 抓包要点
				}
			} else if isSaeFamily(oldEnc) && oldOCV != "" {
				delOpts = append(delOpts, "ocv")
			}
		}
		if network != "" { // 留空 = 保持现状，避免误清 network
			setIf("network", network, secField(*cur, "network"))
		}
		setIf("hidden", hidden, boolUci(secField(*cur, "hidden") != "" && uciTruthy(secField(*cur, "hidden"))))
		setIf("isolate", isolate, boolUci(secField(*cur, "isolate") != "" && uciTruthy(secField(*cur, "isolate"))))
		setIf("disabled", disabled, boolUci(secField(*cur, "disabled") != "" && uciTruthy(secField(*cur, "disabled"))))
	}

	if !isAdd && len(values) == 0 && len(delOpts) == 0 {
		okMsg("参数未变化，无需修改")
		return
	}

	// uci.add（新增）或 uci.set（编辑暂存）——写方法走 CallWriteInto（空响应即成功）
	if isAdd {
		if err := a.Ubus.CallWriteInto(ctx, dev.SessionID, "uci", "add", &map[string]any{},
			map[string]any{"config": "wireless", "type": "wifi-iface", "name": addName, "values": values}); err != nil {
			audit(false, map[string]any{"op": "add", "radio": radio, "name": addName, "values": values, "step": "uci.add", "err": err.Error()})
			errMsg("uci.add 失败: " + err.Error())
			return
		}
	} else {
		if len(values) > 0 {
			if err := a.Ubus.CallWriteInto(ctx, dev.SessionID, "uci", "set", &map[string]any{},
				map[string]any{"config": "wireless", "section": section, "values": values}); err != nil {
				audit(false, map[string]any{"section": section, "radio": radio, "values": values, "step": "uci.set", "err": err.Error()})
				errMsg("uci.set 失败: " + err.Error())
				return
			}
		}
		for _, opt := range delOpts {
			if err := a.Ubus.CallWriteInto(ctx, dev.SessionID, "uci", "delete", &map[string]any{},
				map[string]any{"config": "wireless", "section": section, "option": opt}); err != nil {
				audit(false, map[string]any{"section": section, "radio": radio, "step": "uci.delete", "option": opt, "err": err.Error()})
				errMsg("uci.delete " + opt + " 失败: " + err.Error())
				return
			}
		}
	}

	// 暂存检查（仅日志，与 LuCI 行为一致）
	var changes any
	if err := a.Ubus.CallInto(ctx, dev.SessionID, "uci", "changes", &changes, map[string]any{}); err != nil {
		a.Log.Warn("ssid edit: uci.changes failed", "device_id", deviceID, "err", err)
	} else {
		a.Log.Info("ssid edit: staged changes", "device_id", deviceID, "section", section, "changes", changes)
	}

	// 生效降级链（与 radio 编辑共用，见 finalizeWirelessApply）
	path, msg, detail, ok := a.finalizeWirelessApply(ctx, dev.SessionID, deviceID)
	auditDetail := map[string]any{"section": section, "radio": radio, "values": values, "applied": path}
	if isAdd {
		auditDetail["op"] = "add"
		auditDetail["name"] = addName
	}
	if len(delOpts) > 0 {
		auditDetail["deleted"] = delOpts
	}
	if detail != "" {
		auditDetail["detail"] = detail
	}
	audit(ok, auditDetail)
	if ok {
		if isAdd {
			okMsg("SSID「" + ssid + "」已新增：" + msg)
		} else {
			okMsg("SSID「" + ssid + "」：" + msg)
		}
	} else {
		errMsg(msg)
	}
}

// boolUci 布尔值转 uci 字符串（"1"/"0"）。
func boolUci(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// handleDeviceConfigSSIDDelete 删除 SSID（§5.3；uci.delete section → 生效降级链）。
// 服务端校验 section 存在且为 wifi-iface（不信任前端）；危险操作前端二次确认。
func (a *App) handleDeviceConfigSSIDDelete(c *gin.Context) {
	deviceID := c.Param("id")
	dev, err := a.Frps.GetDevice(c.Request.Context(), deviceID)
	if err != nil {
		c.String(http.StatusNotFound, "设备未找到：%s", deviceID)
		return
	}
	back := "/devices/" + deviceID + "/config?tab=wireless"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&err="+url.QueryEscape(msg)) }

	u, _ := auth.CurrentUser(c)
	audit := func(ok bool, detail map[string]any) {
		action := "wireless.ssid_delete_failed"
		if ok {
			action = "wireless.ssid_delete"
		}
		_ = store.RecordAudit(a.DB, store.AuditEntry{
			UserID: u.ID, Username: u.Username, Action: action,
			Target: deviceID, Detail: detail,
			IP: c.ClientIP(), UserAgent: c.Request.UserAgent(),
		})
	}

	section := strings.TrimSpace(c.PostForm("section"))
	if section == "" {
		section = strings.TrimSpace(c.Query("section"))
	}
	if section == "" {
		errMsg("缺少 section 参数")
		return
	}
	if dev.SessionID == "" || dev.Status != "online" {
		errMsg("设备离线，无法修改配置")
		return
	}
	ctx := c.Request.Context()

	// 校验 section 存在且为 wifi-iface
	secs, err := fetchUciSections(ctx, a, dev.SessionID, "wireless")
	if err != nil {
		audit(false, map[string]any{"section": section, "step": "uci.get wireless", "err": err.Error()})
		errMsg("读取无线配置失败: " + err.Error())
		return
	}
	found := false
	ssidName := section
	for _, s := range secs {
		if s.Name == section && s.Type == "wifi-iface" {
			found = true
			if v := secField(s, "ssid"); v != "" {
				ssidName = v
			}
			break
		}
	}
	if !found {
		audit(false, map[string]any{"section": section, "step": "validate", "err": "unknown section"})
		errMsg("未知的 SSID section：" + section)
		return
	}

	// uci.delete 删除整个 wifi-iface section
	if err := a.Ubus.CallWriteInto(ctx, dev.SessionID, "uci", "delete", &map[string]any{},
		map[string]any{"config": "wireless", "section": section}); err != nil {
		audit(false, map[string]any{"section": section, "step": "uci.delete", "err": err.Error()})
		errMsg("uci.delete 失败: " + err.Error())
		return
	}

	// 暂存检查（仅日志）
	var changes any
	if err := a.Ubus.CallInto(ctx, dev.SessionID, "uci", "changes", &changes, map[string]any{}); err != nil {
		a.Log.Warn("ssid delete: uci.changes failed", "device_id", deviceID, "err", err)
	} else {
		a.Log.Info("ssid delete: staged changes", "device_id", deviceID, "section", section, "changes", changes)
	}

	// 生效降级链（与 radio/SSID 编辑共用）
	path, msg, detail, ok := a.finalizeWirelessApply(ctx, dev.SessionID, deviceID)
	auditDetail := map[string]any{"section": section, "ssid": ssidName, "applied": path}
	if detail != "" {
		auditDetail["detail"] = detail
	}
	audit(ok, auditDetail)
	if ok {
		okMsg("SSID「" + ssidName + "」已删除：" + msg)
	} else {
		errMsg(msg)
	}
}
