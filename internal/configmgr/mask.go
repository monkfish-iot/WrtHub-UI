// Package configmgr 设备 UCI 配置管理：全量快照采集、密钥掩码、关键项定义与比较。
//
// 设计约束（见 配置文件管理设计方案.md）：
//   - 快照链路只读，永不调用 uci set/add/delete/apply；
//   - 密钥 option 入库前替换为固定掩码 "******"，恢复/比较时跳过；
//   - 关键项定义表内置（随版本发布），只取具名 section。
package configmgr

import (
	"strings"
)

// SecretMask 是密钥字段的固定掩码。
const SecretMask = "******"

// secretOptionNames 命中以下名称的 option 视为密钥（小写匹配）。
// 与设计文档 6.2 保持一致。
var secretOptionNames = map[string]struct{}{
	"password":            {},
	"passwd":              {},
	"key":                 {},
	"psk":                 {},
	"wpa_key":             {},
	"shared_secret":       {},
	"primary_auth_secret": {},
	"primary_acct_secret": {},
	"private_key":         {},
	"token":               {},
}

// IsSecretOption 判断 option 名是否为密钥类。
func IsSecretOption(name string) bool {
	_, ok := secretOptionNames[strings.ToLower(name)]
	return ok
}

// SectionValues 是一个 UCI section 的全部键值（含 .type/.name/.index/.anonymous 元字段）。
type SectionValues map[string]any

// PackageValues 是一个配置包的 section 名 → 键值。
type PackageValues map[string]SectionValues

// MaskSecrets 原地将密钥 option 替换为掩码（list 值整体替换为单元素掩码）。
// 返回被掩码的 option 数。
func MaskSecrets(values PackageValues) int {
	masked := 0
	for _, sec := range values {
		for k := range sec {
			if strings.HasPrefix(k, ".") {
				continue
			}
			if IsSecretOption(k) {
				sec[k] = SecretMask
				masked++
			}
		}
	}
	return masked
}
