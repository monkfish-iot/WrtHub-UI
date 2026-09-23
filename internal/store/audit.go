package store

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// AuditEntry 是写入审计日志所需的最小字段集（便于调用方构造）。
type AuditEntry struct {
	UserID    uint
	Username  string
	Action    string // 如 user.login / user.logout / device.view
	Target    string // 操作对象，如用户名/设备ID
	Detail    any    // 任意可 JSON 序列化结构；nil 则记 null
	IP        string
	UserAgent string
}

// RecordAudit 写入一条审计日志。Detail 会 JSON 序列化为 []byte 存入。
// 调用方应容忍错误（审计失败不应阻断主流程）。
func RecordAudit(db *gorm.DB, e AuditEntry) error {
	var detail []byte
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return err
		}
		detail = b
	}
	log := AuditLog{
		UserID:    e.UserID,
		Username:  e.Username,
		Action:    e.Action,
		Target:    e.Target,
		Detail:    detail,
		IP:        e.IP,
		UserAgent: e.UserAgent,
		CreatedAt: time.Now(),
	}
	return db.Create(&log).Error
}
