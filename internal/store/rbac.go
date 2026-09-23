package store

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// defaultPermissions 是系统启动时确保存在的默认权限集（开发技术文档 §5.2 / §6）。
// code 形如 "<module>:<action>"，与 RequirePermission 中间件一致。
var defaultPermissions = []Permission{
	{Code: "device:read", Name: "查看设备", Module: "device", Action: "read"},
	{Code: "device:write", Name: "管理设备", Module: "device", Action: "write"},
	{Code: "wireless:read", Name: "查看无线", Module: "wireless", Action: "read"},
	{Code: "wireless:write", Name: "管理无线", Module: "wireless", Action: "write"},
	{Code: "user:read", Name: "查看用户", Module: "user", Action: "read"},
	{Code: "user:write", Name: "管理用户", Module: "user", Action: "write"},
	{Code: "role:read", Name: "查看角色", Module: "role", Action: "read"},
	{Code: "role:write", Name: "管理角色", Module: "role", Action: "write"},
	{Code: "audit:read", Name: "查看审计", Module: "audit", Action: "read"},
}

// SeedRBAC 幂等地初始化默认权限集，并将全部权限赋予 admin 角色。
// 应在 SeedAdmin 之后调用（依赖 admin 角色已存在）。
func SeedRBAC(db *gorm.DB) error {
	// 1. 创建默认权限（已存在则跳过）
	for _, p := range defaultPermissions {
		var exist Permission
		err := db.Where("code = ?", p.Code).First(&exist).Error
		if err == nil {
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("query permission %s: %w", p.Code, err)
		}
		if err := db.Create(&p).Error; err != nil {
			return fmt.Errorf("create permission %s: %w", p.Code, err)
		}
	}

	// 2. admin 角色赋予全部权限
	var adminRole Role
	if err := db.Where("code = ?", "admin").First(&adminRole).Error; err != nil {
		return fmt.Errorf("find admin role for permission grant: %w", err)
	}
	var perms []Permission
	if err := db.Find(&perms).Error; err != nil {
		return fmt.Errorf("list permissions: %w", err)
	}
	for _, p := range perms {
		rp := RolePermission{RoleID: adminRole.ID, PermissionID: p.ID}
		if err := db.Where("role_id = ? AND permission_id = ?", rp.RoleID, rp.PermissionID).
			FirstOrCreate(&rp).Error; err != nil {
			return fmt.Errorf("ensure role_permission (role=%d, perm=%d): %w", adminRole.ID, p.ID, err)
		}
	}
	return nil
}

// HasPermission 查询用户是否拥有指定权限 code。
// 通过 user_roles → role_permissions → permissions 三表 JOIN 计数。
func HasPermission(db *gorm.DB, userID uint, code string) (bool, error) {
	var cnt int64
	err := db.Model(&Permission{}).
		Joins("JOIN role_permissions ON role_permissions.permission_id = permissions.id").
		Joins("JOIN user_roles ON user_roles.role_id = role_permissions.role_id").
		Where("user_roles.user_id = ? AND permissions.code = ?", userID, code).
		Count(&cnt).Error
	if err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// IsAdmin 判断用户是否拥有 admin 角色（管理员绕过权限检查）。
func IsAdmin(db *gorm.DB, userID uint) (bool, error) {
	var cnt int64
	err := db.Model(&Role{}).
		Joins("JOIN user_roles ON user_roles.role_id = roles.id").
		Where("user_roles.user_id = ? AND roles.code = ?", userID, "admin").
		Count(&cnt).Error
	if err != nil {
		return false, err
	}
	return cnt > 0, nil
}
