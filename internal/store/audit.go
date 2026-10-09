package store

import (
	"time"

	"gorm.io/gorm"
)

// RecordOpLog 追加写入一条操作日志（append-only）。
// 调用方（统一拦截中间件）应容忍错误：日志写入失败不应影响主流程。
func RecordOpLog(db *gorm.DB, rec *AuditLog) error {
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	return db.Create(rec).Error
}