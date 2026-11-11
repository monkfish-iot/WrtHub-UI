// Package store 的平台业务配置（settings 单行表）读写与配置分层。
//
// 配置分层：settings 表为覆盖层，config.yaml 为默认层。
// 首次启动表为空时把文件配置种子进表；此后「系统设置」页只读写本表，
// 文件对应配置项改动不再生效。删除该行并重启即回退文件默认值重新种子。
package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"wrthub-ui/internal/config"
)

// SettingRowID 平台业务配置单行表主键（固定 1）。
const SettingRowID = 1

// Setting 平台业务配置（单行表 settings）。
//
// 仅承载适合在线管理的业务/运行时参数；监听地址、存储、frps_helper 连接信息等
// 引导配置仍保留在 config.yaml。超时类字段以秒存储（UI 表单友好），应用时转 time.Duration。
type Setting struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// PORTAL 模板包解压根目录（按模板 code 子目录）；变更后原目录模板文件不自动迁移
	PortalTemplateRoot string `gorm:"size:255;not null;default:''" json:"portal_template_root"`

	// frps_helper 北向接口单请求超时（秒）
	FrpsRequestTimeoutSec int `gorm:"not null;default:10" json:"frps_request_timeout_sec"`
	// 设备 RPC（lucirpc/ubus 经 frp 隧道）单请求超时（秒）
	DeviceRpcTimeoutSec int `gorm:"not null;default:10" json:"device_rpc_timeout_sec"`
	// cgi-io 固件上传专用超时（秒，大文件慢操作，远大于普通超时）
	DeviceRpcUploadTimeoutSec int `gorm:"not null;default:1800" json:"device_rpc_upload_timeout_sec"`
	// 设备 token / 登录凭证内存缓存 TTL（秒）
	DeviceRpcTokenCacheTTLSec int `gorm:"not null;default:300" json:"device_rpc_token_cache_ttl_sec"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Setting) TableName() string { return "settings" }

// SettingFromConfig 文件配置 → 配置行（首次启动种子用；cfg 须已经过 applyDefaults）。
func SettingFromConfig(cfg *config.Config) *Setting {
	return &Setting{
		ID:                        SettingRowID,
		PortalTemplateRoot:        cfg.Portal.TemplateRoot,
		FrpsRequestTimeoutSec:     int(cfg.FrpsHelper.RequestTimeout / time.Second),
		DeviceRpcTimeoutSec:       int(cfg.DeviceRpc.Timeout / time.Second),
		DeviceRpcUploadTimeoutSec: int(cfg.DeviceRpc.UploadTimeout / time.Second),
		DeviceRpcTokenCacheTTLSec: int(cfg.DeviceRpc.TokenCacheTTL / time.Second),
	}
}

// ApplyToConfig 把 DB 配置行覆盖到运行时配置（超时秒 → time.Duration）。
func (s *Setting) ApplyToConfig(cfg *config.Config) {
	cfg.Portal.TemplateRoot = s.PortalTemplateRoot
	cfg.FrpsHelper.RequestTimeout = time.Duration(s.FrpsRequestTimeoutSec) * time.Second
	cfg.DeviceRpc.Timeout = time.Duration(s.DeviceRpcTimeoutSec) * time.Second
	cfg.DeviceRpc.UploadTimeout = time.Duration(s.DeviceRpcUploadTimeoutSec) * time.Second
	cfg.DeviceRpc.TokenCacheTTL = time.Duration(s.DeviceRpcTokenCacheTTLSec) * time.Second
}

// LoadSettings 读取平台业务配置行；不存在时返回 (nil, nil)。
func LoadSettings(db *gorm.DB) (*Setting, error) {
	var s Setting
	err := db.First(&s, SettingRowID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SeedSettings 表为空时按文件配置种子；返回是否执行了种子写入。
func SeedSettings(db *gorm.DB, cfg *config.Config) (bool, error) {
	var cnt int64
	if err := db.Model(&Setting{}).Where("id = ?", SettingRowID).Count(&cnt).Error; err != nil {
		return false, fmt.Errorf("count settings: %w", err)
	}
	if cnt > 0 {
		return false, nil
	}
	if err := db.Create(SettingFromConfig(cfg)).Error; err != nil {
		return false, fmt.Errorf("seed settings: %w", err)
	}
	return true, nil
}

// SaveSettings 覆盖写平台业务配置行（不存在则创建）。
// 显式全列更新：空串/0 等零值也必须落库（不能用默认的非零值 Updates 语义）。
func SaveSettings(db *gorm.DB, s *Setting) error {
	s.ID = SettingRowID
	var cnt int64
	if err := db.Model(&Setting{}).Where("id = ?", SettingRowID).Count(&cnt).Error; err != nil {
		return err
	}
	if cnt == 0 {
		return db.Create(s).Error
	}
	return db.Model(&Setting{}).Where("id = ?", SettingRowID).
		Select("*").Omit("id", "created_at").Updates(s).Error
}
