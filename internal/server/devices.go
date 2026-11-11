package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

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
	FlashOK    string
	FlashErr   string
	// AutoRegister 自动注册开关（frps_helper 运行时状态，开启时隐藏预注册入口）
	AutoRegister bool
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

	autoReg, err := a.Frps.GetAutoRegister(c.Request.Context())
	if err != nil {
		// 查询失败按关闭处理（预注册入口保留），不阻断列表页
		a.Log.Error("get auto_register failed", "err", err)
		autoReg = false
	}

	u, _ := auth.CurrentUser(c)
	pg := devicesListPage{
		pageBase:     pageBase{ActiveMenu: "devices", CurrentUser: u, Config: a.Cfg},
		Devices:      items,
		Total:        total,
		Page:         page,
		PageSize:     pageSize,
		TotalPages:   totalPages,
		Filters:      filters,
		Pages:        buildPageLinks(page, totalPages, filters),
		FlashOK:      c.Query("ok"),
		FlashErr:     c.Query("err"),
		AutoRegister: autoReg,
	}
	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/devices/list", pg); rerr != nil {
		a.Log.Error("render devices list failed", "err", rerr)
	}
}

// handleDevicePreRegister 预注册设备（POST /devices/pre-register）。
// 单租户化对应 tentant-mng 的"绑定设备"：按 device_id 在 frps_helper 创建占位记录，
// 其余字段（MAC/名称/型号）可选，设备上线 join 时自动补齐。
func (a *App) handleDevicePreRegister(c *gin.Context) {
	back := "/devices"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+url.QueryEscape(msg)) }

	deviceID := c.PostForm("device_id")
	if deviceID == "" {
		errMsg("预注册失败：设备 ID 不能为空")
		return
	}
	req := &frpsclient.PreRegisterDeviceRequest{
		DeviceID:   deviceID,
		MAC:        c.PostForm("mac"),
		DeviceName: c.PostForm("device_name"),
		Model:      c.PostForm("model"),
	}
	dev, err := a.Frps.PreRegisterDevice(c.Request.Context(), req)
	if err != nil {
		a.Log.Error("pre-register device failed", "device_id", deviceID, "err", err)
		errMsg("预注册失败：" + err.Error())
		return
	}
	okMsg("设备 " + deviceID + " 已预注册（当前状态：" + dev.Status + "）")
}

// handleDeviceAutoRegister 切换设备自动注册开关（POST /devices/auto-register）。
// 开启后设备可自行 join 接入；关闭后仅预注册设备可接入（frps_helper 运行时生效，
// 重启后恢复其配置文件值）。UI 上与"预注册设备"入口互斥：开启时隐藏预注册按钮。
func (a *App) handleDeviceAutoRegister(c *gin.Context) {
	back := "/devices"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+url.QueryEscape(msg)) }

	enabled := c.PostForm("enabled") == "1"
	if err := a.Frps.SetAutoRegister(c.Request.Context(), enabled); err != nil {
		a.Log.Error("set auto_register failed", "enabled", enabled, "err", err)
		if enabled {
			errMsg("开启自动注册失败：" + err.Error())
		} else {
			errMsg("关闭自动注册失败：" + err.Error())
		}
		return
	}
	if enabled {
		okMsg("自动注册已开启：新设备可自行接入，无需预注册")
	} else {
		okMsg("自动注册已关闭：仅预注册设备可接入")
	}
}

// handleDeviceDelete 删除设备（POST /devices/delete）。
// 依赖 frps_helper 的软删除接口；在线设备由服务端拒绝。
func (a *App) handleDeviceDelete(c *gin.Context) {
	back := "/devices"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+url.QueryEscape(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+url.QueryEscape(msg)) }

	deviceID := strings.TrimSpace(c.PostForm("device_id"))
	if deviceID == "" {
		errMsg("删除失败：设备 ID 不能为空")
		return
	}
	if err := a.Frps.DeleteDevice(c.Request.Context(), deviceID); err != nil {
		a.Log.Error("delete device failed", "device_id", deviceID, "err", err)
		errMsg("删除设备 " + deviceID + " 失败：" + err.Error())
		return
	}
	okMsg("设备 " + deviceID + " 已删除")
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
