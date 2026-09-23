// Package store 提供 WrtHub-UI 自有的持久化层：GORM 模型与数据库初始化。
//
// 设备数据不本地存储（实时查 frps_helper）；本层仅承载用户/角色/权限/审计/租户映射。
// 驱动可在 SQLite 与 PostgreSQL 间切换（见 Store.Open）。
package store

import (
	"time"
)

// User 平台用户。密码以 bcrypt hash 存储，明文永不落盘。
type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"uniqueIndex;size:64;not null" json:"username"`
	PasswordHash string    `gorm:"size:128;not null" json:"-"`
	Email        string    `gorm:"size:128" json:"email"`
	Status       int       `gorm:"default:1;not null" json:"status"` // 0 禁用 1 启用
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// 多对多关联由中间表 user_roles 维护（见 UserRole）。
	Roles []Role `gorm:"many2many:user_roles;" json:"roles,omitempty"`
}

func (User) TableName() string { return "users" }

// Role 角色。code 为业务标识（如 admin/operator/viewer）。
type Role struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"size:64;not null" json:"name"`
	Code        string    `gorm:"uniqueIndex;size:64;not null" json:"code"`
	Description string    `gorm:"size:255" json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	// 多对多关联由中间表 role_permissions 维护（见 RolePermission）。
	Permissions []Permission `gorm:"many2many:role_permissions;" json:"permissions,omitempty"`
}

func (Role) TableName() string { return "roles" }

// Permission 权限。粒度到 模块+动作，code 形如 device:read / wifi:write。
type Permission struct {
	ID     uint   `gorm:"primaryKey" json:"id"`
	Code   string `gorm:"uniqueIndex;size:64;not null" json:"code"`
	Name   string `gorm:"size:128" json:"name"`
	Module string `gorm:"size:32;index" json:"module"`
	Action string `gorm:"size:32" json:"action"`
}

func (Permission) TableName() string { return "permissions" }

// UserRole 用户-角色映射（中间表，显式定义以便 RBAC 直接维护）。
type UserRole struct {
	UserID uint `gorm:"primaryKey"`
	RoleID uint `gorm:"primaryKey"`
}

func (UserRole) TableName() string { return "user_roles" }

// RolePermission 角色-权限映射（中间表）。
type RolePermission struct {
	RoleID       uint `gorm:"primaryKey"`
	PermissionID uint `gorm:"primaryKey"`
}

func (RolePermission) TableName() string { return "role_permissions" }

// UserTenant 用户-租户映射。tenant_id 对应 frps_helper 的 tenant_org_id，默认 default。
// 多租户过滤能力待 frps_helper 支持后启用，本表先做预留。
type UserTenant struct {
	UserID    uint   `gorm:"primaryKey"`
	TenantID  string `gorm:"primaryKey;size:64"`
	IsDefault bool   `gorm:"default:false"`
}

func (UserTenant) TableName() string { return "user_tenants" }

// AuditLog 审计日志。记录登录、设备控制、配置变更等关键动作。
// Detail 为任意 JSON（json.RawMessage 便于结构化查询但不强约束）。
type AuditLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"index" json:"user_id"`
	Username  string    `gorm:"size:64" json:"username"` // 冗余便于用户删除后仍可追溯
	Action    string    `gorm:"size:64;index" json:"action"`
	Target    string    `gorm:"size:128" json:"target"`
	Detail    []byte    `gorm:"type:json" json:"detail"`
	IP        string    `gorm:"size:64" json:"ip"`
	UserAgent string    `gorm:"size:255" json:"user_agent"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

func (AuditLog) TableName() string { return "audit_logs" }

// allModels 返回待迁移的全部模型，便于 AutoMigrate 统一管理。
func allModels() []any {
	return []any{
		&User{},
		&Role{},
		&Permission{},
		&UserRole{},
		&RolePermission{},
		&UserTenant{},
		&AuditLog{},
	}
}

// 用户状态常量。
const (
	UserStatusDisabled = 0
	UserStatusActive   = 1
)
