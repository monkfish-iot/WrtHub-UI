package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/store"
)

// opLogFilters 是操作日志列表的过滤参数（来自 query）。
type opLogFilters struct {
	Module   string
	Action   string
	Operator string
	TargetID string
	Result   string
	TimeFrom string // YYYY-MM-DD
	TimeTo   string // YYYY-MM-DD
}

// opLogListPage 是操作日志列表页视图。
type opLogListPage struct {
	pageBase
	Logs       []store.AuditLog
	Total      int64
	Page       int
	PageSize   int
	TotalPages int
	Filters    opLogFilters
	Pages      []pageLink
}

// handleAuditList 渲染操作日志列表页（按时间倒序，支持多维筛选与分页）。
func (a *App) handleAuditList(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	pageSize := atoiDefault(c.Query("page_size"), 20)
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}

	filters := opLogFilters{
		Module:   strings.TrimSpace(c.Query("module")),
		Action:   strings.TrimSpace(c.Query("action")),
		Operator: strings.TrimSpace(c.Query("operator")),
		TargetID: strings.TrimSpace(c.Query("target_id")),
		Result:   strings.TrimSpace(c.Query("result")),
		TimeFrom: strings.TrimSpace(c.Query("time_from")),
		TimeTo:   strings.TrimSpace(c.Query("time_to")),
	}

	q := a.DB.Model(&store.AuditLog{})
	if filters.Module != "" {
		q = q.Where("module = ?", filters.Module)
	}
	if filters.Action != "" {
		q = q.Where("action = ?", filters.Action)
	}
	if filters.Operator != "" {
		q = q.Where("username LIKE ?", "%"+filters.Operator+"%")
	}
	if filters.TargetID != "" {
		q = q.Where("target_id LIKE ?", "%"+filters.TargetID+"%")
	}
	if filters.Result != "" {
		q = q.Where("result = ?", filters.Result)
	}
	if from, ok := parseDayStart(filters.TimeFrom); ok {
		q = q.Where("created_at >= ?", from)
	}
	if to, ok := parseDayEnd(filters.TimeTo); ok {
		q = q.Where("created_at <= ?", to)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		a.Log.Error("count operation logs failed", "err", err)
	}
	var logs []store.AuditLog
	if err := q.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&logs).Error; err != nil {
		a.Log.Error("list operation logs failed", "err", err)
	}
	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	u, _ := auth.CurrentUser(c)
	pg := opLogListPage{
		pageBase:   pageBase{ActiveMenu: "oplogs", CurrentUser: u, Config: a.Cfg},
		Logs:       logs,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages,
		Filters:    filters,
		Pages:      buildOpLogPageLinks(page, totalPages, filters),
	}
	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/audit/list", pg); rerr != nil {
		a.Log.Error("render operation logs failed", "err", rerr)
	}
}

// parseDayStart 解析 YYYY-MM-DD 为该日 00:00:00（本地时区）。
func parseDayStart(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// parseDayEnd 解析 YYYY-MM-DD 为该日 23:59:59（本地时区）。
func parseDayEnd(s string) (time.Time, bool) {
	t, ok := parseDayStart(s)
	if !ok {
		return time.Time{}, false
	}
	return t.Add(24*time.Hour - time.Second), true
}

// buildOpLogPageLinks 构造分页条目：« + 首页 + 省略号 + 当前±2 + 省略号 + 末页 + »。
func buildOpLogPageLinks(page, total int, f opLogFilters) []pageLink {
	links := make([]pageLink, 0, 10)
	links = append(links, pageLink{Label: "«", URL: opLogPageURL(page-1, f), Disabled: page <= 1})

	start := page - 2
	if start < 1 {
		start = 1
	}
	end := page + 2
	if end > total {
		end = total
	}
	if start > 1 {
		links = append(links, pageLink{Label: "1", URL: opLogPageURL(1, f)})
		if start > 2 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
	}
	for i := start; i <= end; i++ {
		links = append(links, pageLink{Label: strconv.Itoa(i), URL: opLogPageURL(i, f), Active: i == page})
	}
	if end < total {
		if end < total-1 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
		links = append(links, pageLink{Label: strconv.Itoa(total), URL: opLogPageURL(total, f)})
	}
	links = append(links, pageLink{Label: "»", URL: opLogPageURL(page+1, f), Disabled: page >= total})
	return links
}

func opLogPageURL(n int, f opLogFilters) string {
	q := url.Values{}
	if f.Module != "" {
		q.Set("module", f.Module)
	}
	if f.Action != "" {
		q.Set("action", f.Action)
	}
	if f.Operator != "" {
		q.Set("operator", f.Operator)
	}
	if f.TargetID != "" {
		q.Set("target_id", f.TargetID)
	}
	if f.Result != "" {
		q.Set("result", f.Result)
	}
	if f.TimeFrom != "" {
		q.Set("time_from", f.TimeFrom)
	}
	if f.TimeTo != "" {
		q.Set("time_to", f.TimeTo)
	}
	q.Set("page", strconv.Itoa(n))
	return "/operation-logs?" + q.Encode()
}