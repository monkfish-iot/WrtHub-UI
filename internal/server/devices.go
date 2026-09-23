package server

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/config"
	"wrthub-ui/internal/frpsclient"
	"wrthub-ui/internal/store"
)

// pageBase 是所有受保护页面的公共视图字段，供 layout 使用。
type pageBase struct {
	ActiveMenu  string
	CurrentUser *store.User
	Config      *config.Config
}

// deviceFilters 是设备列表的过滤参数（来自 query）。
type deviceFilters struct {
	Keyword string
	Status  string
	Model   string
}

// pageLink 是分页条目。
type pageLink struct {
	Label    string
	URL      string
	Active   bool
	Disabled bool
}

// devicesListPage 是设备列表页视图。
type devicesListPage struct {
	pageBase
	Devices    []frpsclient.Device
	Total      int
	Page       int
	PageSize   int
	TotalPages int
	Filters    deviceFilters
	Pages      []pageLink
}

// handleDevicesList 渲染设备列表页（HTMX 局部刷新待 todo8 引入 HTMX 后升级；当前整页 GET）。
func (a *App) handleDevicesList(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	pageSize := atoiDefault(c.Query("page_size"), 15)
	if pageSize < 1 {
		pageSize = 15
	}
	if pageSize > 100 {
		pageSize = 100
	}
	if page < 1 {
		page = 1
	}

	filters := deviceFilters{
		Keyword: c.Query("keyword"),
		Status:  c.Query("status"),
		Model:   c.Query("model"),
	}
	res, err := a.Frps.ListDevices(c.Request.Context(), frpsclient.ListOptions{
		Page:     page,
		PageSize: pageSize,
		Status:   filters.Status,
		Keyword:  filters.Keyword,
		Model:    filters.Model,
	})
	var total int
	var items []frpsclient.Device
	if err == nil && res != nil {
		total = res.Total
		items = res.Items
	} else if err != nil {
		a.Log.Error("list devices failed", "err", err)
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	u, _ := auth.CurrentUser(c)
	pg := devicesListPage{
		pageBase:   pageBase{ActiveMenu: "devices", CurrentUser: u, Config: a.Cfg},
		Devices:    items,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages,
		Filters:    filters,
		Pages:      buildPageLinks(page, totalPages, filters),
	}
	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/devices/list", pg); rerr != nil {
		a.Log.Error("render devices list failed", "err", rerr)
	}
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return def
	}
	return n
}

// buildPageLinks 构造分页条目：« + 首页 + 省略号 + 当前±2 + 省略号 + 末页 + »。
func buildPageLinks(page, total int, f deviceFilters) []pageLink {
	links := make([]pageLink, 0, 10)
	links = append(links, pageLink{Label: "«", URL: pageURL(page-1, f), Disabled: page <= 1})

	start := page - 2
	if start < 1 {
		start = 1
	}
	end := page + 2
	if end > total {
		end = total
	}
	if start > 1 {
		links = append(links, pageLink{Label: "1", URL: pageURL(1, f)})
		if start > 2 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
	}
	for i := start; i <= end; i++ {
		links = append(links, pageLink{Label: strconv.Itoa(i), URL: pageURL(i, f), Active: i == page})
	}
	if end < total {
		if end < total-1 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
		links = append(links, pageLink{Label: strconv.Itoa(total), URL: pageURL(total, f)})
	}
	links = append(links, pageLink{Label: "»", URL: pageURL(page+1, f), Disabled: page >= total})
	return links
}

func pageURL(n int, f deviceFilters) string {
	q := url.Values{}
	if f.Keyword != "" {
		q.Set("keyword", f.Keyword)
	}
	if f.Status != "" {
		q.Set("status", f.Status)
	}
	if f.Model != "" {
		q.Set("model", f.Model)
	}
	q.Set("page", strconv.Itoa(n))
	return "/devices?" + q.Encode()
}
