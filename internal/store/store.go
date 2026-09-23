package store

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open 按 config.driver 打开数据库并执行 AutoMigrate。
// 当前实现支持 sqlite（纯 Go 驱动 glebarez/sqlite，无 cgo，便于交叉编译）；
// postgres 需额外引入 gorm.io/driver/postgres，待切换时再补。
func Open(driver, sqlitePath, postgresDSN string) (*gorm.DB, error) {
	var db *gorm.DB
	var err error
	switch driver {
	case "sqlite":
		// 确保数据目录存在，避免 SQLite 打开失败。
		if dir := filepath.Dir(sqlitePath); dir != "" && dir != "." {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return nil, fmt.Errorf("create sqlite dir: %w", mkErr)
			}
		}
		db, err = gorm.Open(sqlite.Open(sqlitePath), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Warn),
		})
	case "postgres":
		return nil, errors.New("postgres driver not imported yet; run `go get gorm.io/driver/postgres` and rebuild")
	default:
		return nil, fmt.Errorf("unknown storage driver: %q", driver)
	}
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := db.AutoMigrate(allModels()...); err != nil {
		return nil, fmt.Errorf("automigrate: %w", err)
	}
	return db, nil
}

// SeedAdmin 首次启动若无任何用户，则按 initUsername/initPassword 创建管理员并赋予 admin 角色。
// 已存在用户则跳过（幂等）。默认密码会在日志中以 WARN 提示尽快修改。
func SeedAdmin(db *gorm.DB, initUsername, initPassword string, log *slog.Logger) error {
	var cnt int64
	if err := db.Model(&User{}).Count(&cnt).Error; err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if cnt > 0 {
		return nil
	}

	hash, err := HashPassword(initPassword)
	if err != nil {
		return fmt.Errorf("hash init password: %w", err)
	}

	// 确保 admin 角色存在（与用户创建同事务）。
	var role Role
	if err := db.Where("code = ?", "admin").First(&role).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			role = Role{Name: "Administrator", Code: "admin", Description: "system built-in administrator"}
			if err := db.Create(&role).Error; err != nil {
				return fmt.Errorf("create admin role: %w", err)
			}
		} else {
			return fmt.Errorf("find admin role: %w", err)
		}
	}

	u := User{
		Username:     initUsername,
		PasswordHash: hash,
		Status:       UserStatusActive,
		Roles:        []Role{role},
	}
	if err := db.Create(&u).Error; err != nil {
		return fmt.Errorf("create admin user: %w", err)
	}

	if log != nil {
		log.Warn("initial admin user created; please change the password immediately after first login",
			"username", initUsername)
	}
	return nil
}
