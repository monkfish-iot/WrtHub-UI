package configmgr

import (
	"fmt"
	"sort"
	"strings"
)

// 关键项比较规则
const (
	RuleMustEqual = "must_equal"
	RuleMustIn    = "must_in"
	RuleNotEmpty  = "not_empty"
	RuleRange     = "range"
)

// 期望值来源
const (
	ExpectedSourceBaseline = "baseline" // 设备首读值
	ExpectedSourceTemplate = "template" // 标准值
)

// 变更生效级别
const (
	LevelReload  = "reload"
	LevelRestart = "restart"
	LevelReboot  = "reboot"
	LevelManual  = "manual"
)

// KeyItemDef 关键项定义（内置表，随版本发布；设计文档 5.2 / 7.1）。
// 只允许具名 section；密钥 option 一律不入表。
type KeyItemDef struct {
	Key            string // config.section.option
	Config         string
	Section        string
	Option         string
	ValueType      string // string / int / bool / enum / list
	Rule           string
	ExpectedSource string
	ExpectedValue  string // template 时的标准值
	Required       bool
	Repair         bool
	Level          string
	Description    string
}

// KeyItemDefs 内置关键项初版清单（设计文档 7.1）。
var KeyItemDefs = []KeyItemDef{
	{Key: "webauthd.generic.enabled", Config: "webauthd", Section: "generic", Option: "enabled", ValueType: "bool", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: true, Repair: true, Level: LevelRestart, Description: "Portal 服务开关"},
	{Key: "webauthd.generic.layer", Config: "webauthd", Section: "generic", Option: "layer", ValueType: "enum", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: true, Repair: true, Level: LevelRestart, Description: "认证层级"},
	{Key: "webauthd.generic.portal_interface", Config: "webauthd", Section: "generic", Option: "portal_interface", ValueType: "string", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelRestart, Description: "Portal 接口"},
	{Key: "webauthd.generic.mac_auth", Config: "webauthd", Section: "generic", Option: "mac_auth", ValueType: "bool", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelRestart, Description: "MAC 免密开关"},
	{Key: "webauthd.generic.auth_source", Config: "webauthd", Section: "generic", Option: "auth_source", ValueType: "enum", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelRestart, Description: "认证源"},
	{Key: "webauthd.portal.portal_type", Config: "webauthd", Section: "portal", Option: "portal_type", ValueType: "enum", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelReload, Description: "Portal 类型"},
	{Key: "webauthd.portal.portal_url", Config: "webauthd", Section: "portal", Option: "portal_url", ValueType: "string", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelReload, Description: "Portal 地址"},
	{Key: "webauthd.radius.primary_auth_server", Config: "webauthd", Section: "radius", Option: "primary_auth_server", ValueType: "string", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelRestart, Description: "RADIUS 认证服务器"},
	{Key: "webauthd.radius.primary_auth_port", Config: "webauthd", Section: "radius", Option: "primary_auth_port", ValueType: "int", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelRestart, Description: "RADIUS 认证端口"},
	{Key: "webauthd.radius.acct_interval", Config: "webauthd", Section: "radius", Option: "acct_interval", ValueType: "int", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelRestart, Description: "计费间隔"},
	{Key: "network.lan.proto", Config: "network", Section: "lan", Option: "proto", ValueType: "enum", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: true, Repair: false, Level: LevelManual, Description: "LAN 协议"},
	{Key: "network.lan.ipaddr", Config: "network", Section: "lan", Option: "ipaddr", ValueType: "string", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: true, Repair: false, Level: LevelManual, Description: "LAN 地址"},
	{Key: "wireless.radio0.channel", Config: "wireless", Section: "radio0", Option: "channel", ValueType: "int", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelReboot, Description: "信道"},
	{Key: "wireless.radio0.htmode", Config: "wireless", Section: "radio0", Option: "htmode", ValueType: "enum", Rule: RuleMustEqual, ExpectedSource: ExpectedSourceBaseline, Required: false, Repair: true, Level: LevelReboot, Description: "频宽模式"},
}

// keyItemDefIndex key → 定义，启动时构建。
var keyItemDefIndex = func() map[string]KeyItemDef {
	m := make(map[string]KeyItemDef, len(KeyItemDefs))
	for _, d := range KeyItemDefs {
		m[d.Key] = d
	}
	return m
}()

// LookupKeyItemDef 按 key 查定义。
func LookupKeyItemDef(key string) (KeyItemDef, bool) {
	d, ok := keyItemDefIndex[key]
	return d, ok
}

// 关键项状态
const (
	ItemStatusOK      = "ok"
	ItemStatusDrift   = "drift"
	ItemStatusMissing = "missing"
	ItemStatusNA      = "na"
	ItemStatusError   = "error"
)

// ItemResult 是关键项比较结果。
type ItemResult struct {
	Key           string
	Value         string // 设备当前值（掩码项不存真实值；定义表本身不含密钥项）
	ExpectedValue string
	Status        string
}

// normalize 按 value_type 归一化字符串值（设计文档 8.1）。
func normalize(valueType, raw string) string {
	v := strings.TrimSpace(raw)
	switch valueType {
	case "bool":
		switch strings.ToLower(v) {
		case "1", "on", "true", "yes", "enabled":
			return "1"
		case "0", "off", "false", "no", "disabled", "":
			return "0"
		}
		return v
	case "int":
		// 忽略前导零：转十进制文本
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return fmt.Sprintf("%d", n)
		}
		return v
	case "enum":
		return strings.ToLower(v)
	default: // string
		return v
	}
}

// rawValueToString 将 uci 值（string 或 []any list）转比较用字符串。
// list 默认顺序无关：排序后以 "\n" 连接。
func rawValueToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, fmt.Sprintf("%v", e))
		}
		sort.Strings(parts)
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", t)
	}
}

// ExtractAndCompare 从快照 packages 中抽取关键项并与期望值比较（设计文档 8）。
// expected 参数：key → 期望值；nil 表示用定义表 template 值（baseline 项无模板值时记 na）。
func ExtractAndCompare(pkgs map[string]PackageValues, expected map[string]string) []ItemResult {
	results := make([]ItemResult, 0, len(KeyItemDefs))
	for _, def := range KeyItemDefs {
		res := ItemResult{Key: def.Key}

		exp, hasExp := expected[def.Key]
		if !hasExp {
			if def.ExpectedSource == ExpectedSourceTemplate && def.ExpectedValue != "" {
				exp, hasExp = def.ExpectedValue, true
			}
		}
		if !hasExp {
			// baseline 项尚无基线 → na
			res.Status = ItemStatusNA
			results = append(results, res)
			continue
		}
		res.ExpectedValue = normalize(def.ValueType, exp)

		pkg, ok := pkgs[def.Config]
		if !ok {
			if def.Required {
				res.Status = ItemStatusMissing
			} else {
				res.Status = ItemStatusNA
			}
			results = append(results, res)
			continue
		}
		sec, ok := pkg[def.Section]
		if !ok {
			if def.Required {
				res.Status = ItemStatusMissing
			} else {
				res.Status = ItemStatusNA
			}
			results = append(results, res)
			continue
		}
		raw, ok := sec[def.Option]
		if !ok {
			if def.Required {
				res.Status = ItemStatusMissing
			} else {
				res.Status = ItemStatusNA
			}
			results = append(results, res)
			continue
		}
		res.Value = normalize(def.ValueType, rawValueToString(raw))
		if res.Value == res.ExpectedValue {
			res.Status = ItemStatusOK
		} else {
			res.Status = ItemStatusDrift
		}
		results = append(results, res)
	}
	return results
}
