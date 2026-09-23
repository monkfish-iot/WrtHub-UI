package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/store"
)

// auditListPage 是审计日志列表页视图。
type auditListPage struct {
	pageBase
	Logs       []store.AuditLog
	Page       int
	TotalPages int
	PrevPage   int // 0 表示无上一页
	NextPage   int // 0 表示无下一页
}

// handleAuditList 渲染审计日志列表页（按时间倒序，分页）。
func (a *App) handleAuditList(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	pageSize := atoiDefault(c.Query("page_size"), 20)
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}
	if page < 1 {
		page = 1
	}

	var total int64
	if err := a.DB.Model(&store.AuditLog{}).Count(&total).Error; err != nil {
		a.Log.Error("count audit logs failed", "err", err)
	}
	var logs []store.AuditLog
	if err := a.DB.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&logs).Error; err != nil {
		a.Log.Error("list audit logs failed", "err", err)
	}
	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	if totalPages < 1 {
		totalPages = 1
	}

	prev := 0
	if page > 1 {
		prev = page - 1
	}
	next := 0
	if page < totalPages {
		next = page + 1
	}

	u, _ := auth.CurrentUser(c)
	pg := auditListPage{
		pageBase:   pageBase{ActiveMenu: "audit", CurrentUser: u, Config: a.Cfg},
		Logs:       logs,
		Page:       page,
		TotalPages: totalPages,
		PrevPage:   prev,
		NextPage:   next,
	}
	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/audit/list", pg); rerr != nil {
		a.Log.Error("render audit list failed", "err", rerr)
	}
}
