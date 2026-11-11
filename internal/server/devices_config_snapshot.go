package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/configmgr"
	"wrthub-ui/internal/frpsclient"
)

// 设备配置快照（P1：采集/列表/详情/下载/删除）。
// 快照链路只读：仅 uci configs / uci get，绝不调用写接口（设计文档 6.5）。

// configSnapshotsPage 快照列表页视图。
type configSnapshotsPage struct {
	pageBase
	Device      *frpsclient.Device
	Snapshots   []frpsclient.ConfigSnapshot
	Total       int64
	Page        int
	PageSize    int
	TotalPages  int
	Pages       []pageLink
	Policy      *frpsclient.ConfigPolicy
	FlashOK     string
	FlashErr    string
	SourceLabel func(string) string
}

// configSnapshotDetailPage 快照详情页视图。
type configSnapshotDetailPage struct {
	pageBase
	Device      *frpsclient.Device
	Snapshot    *frpsclient.ConfigSnapshot
	PrettyJSON  string // 格式化后的快照 JSON（掩码已生效）
	PackageRows []packageRow
	SourceLabel func(string) string
}

// packageRow 详情页按包名排序的 section 计数行。
type packageRow struct {
	Name     string
	Sections int
}

// snapshotSourceLabel 快照来源中文标签。
func snapshotSourceLabel(s string) string {
	switch s {
	case "first_online":
		return "首次上线"
	case "manual":
		return "手动采集"
	case "pre_restore":
		return "恢复前"
	case "scheduled":
		return "周期采集"
	default:
		return s
	}
}

// handleConfigSnapshots GET /devices/:id/config/snapshots 快照列表页。
func (a *App) handleConfigSnapshots(c *gin.Context) {
	deviceID := c.Param("id")
	dev, err := a.Frps.GetDevice(c.Request.Context(), deviceID)
	if err != nil {
		a.Log.Warn("device not found", "device_id", deviceID, "err", err)
		c.String(http.StatusNotFound, "设备未找到：%s", deviceID)
		return
	}

	page := atoiDefault(c.Query("page"), 1)
	pageSize := 20

	u, _ := auth.CurrentUser(c)
	pg := configSnapshotsPage{
		pageBase:    pageBase{ActiveMenu: "devices", CurrentUser: u, Config: a.Cfg},
		Device:      dev,
		Page:        page,
		PageSize:    pageSize,
		FlashOK:     c.Query("ok"),
		FlashErr:    c.Query("err"),
		SourceLabel: snapshotSourceLabel,
	}

	ctx := c.Request.Context()
	if pol, perr := a.Frps.GetConfigPolicy(ctx, deviceID); perr != nil {
		a.Log.Warn("config policy fetch failed", "device_id", deviceID, "err", perr)
	} else {
		pg.Policy = pol
	}

	list, lerr := a.Frps.ListConfigSnapshots(ctx, deviceID, page, pageSize)
	if lerr != nil {
		pg.FlashErr = "快照列表获取失败: " + lerr.Error()
	} else {
		pg.Snapshots = list.Items
		pg.Total = list.Total
	}
	pg.TotalPages = int((pg.Total + int64(pageSize) - 1) / int64(pageSize))
	if pg.TotalPages < 1 {
		pg.TotalPages = 1
	}
	pg.Pages = buildSnapshotPageLinks(page, pg.TotalPages, func(p int) string {
		return fmt.Sprintf("/devices/%s/config/snapshots?page=%d", deviceID, p)
	})

	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/devices/snapshots", pg); rerr != nil {
		a.Log.Error("render snapshots failed", "err", rerr)
	}
}

// handleConfigSnapshotCapture POST /devices/:id/config/snapshots/capture 手动采集。
// 只读操作（uci configs/get），但属管理动作，要求 device:write 权限。
func (a *App) handleConfigSnapshotCapture(c *gin.Context) {
	deviceID := c.Param("id")
	redirectBack := func(ok, errMsg string) {
		q := ""
		if ok != "" {
			q = "?ok=" + urlQueryEscape(ok)
		} else if errMsg != "" {
			q = "?err=" + urlQueryEscape(errMsg)
		}
		c.Redirect(http.StatusFound, "/devices/"+deviceID+"/config/snapshots"+q)
	}

	dev, err := a.Frps.GetDevice(c.Request.Context(), deviceID)
	if err != nil {
		redirectBack("", "设备未找到: "+deviceID)
		return
	}
	if dev.SessionID == "" || dev.Status != "online" {
		redirectBack("", "设备离线，无法采集配置")
		return
	}

	ctx := c.Request.Context()
	// 策略检查：snapshot_enabled=0 时拒绝采集
	if pol, perr := a.Frps.GetConfigPolicy(ctx, deviceID); perr == nil && pol.SnapshotEnabled == 0 {
		redirectBack("", "该设备已禁用配置快照（snapshot_enabled=0）")
		return
	}

	meta := configmgr.SnapshotMeta{
		DeviceID:   dev.DeviceID,
		Model:      dev.Model,
		Firmware:   "",
		CapturedAt: time.Now(),
		Source:     "manual",
	}
	snap, partial, cerr := configmgr.Collect(ctx, a.Ubus, dev.SessionID, meta)
	if cerr != nil {
		a.Log.Warn("config snapshot collect failed", "device_id", deviceID, "err", cerr)
		redirectBack("", "采集失败: "+cerr.Error())
		return
	}
	if len(partial) > 0 {
		a.Log.Warn("config snapshot partial errors", "device_id", deviceID, "errors", partial)
	}

	content, merr := snap.Marshal()
	if merr != nil {
		redirectBack("", "快照序列化失败: "+merr.Error())
		return
	}
	sha, herr := snap.HashSHA256()
	if herr != nil {
		redirectBack("", "快照指纹计算失败: "+herr.Error())
		return
	}

	res, serr := a.Frps.CreateConfigSnapshot(ctx, &frpsclient.CreateConfigSnapshotRequest{
		TenantOrgID:  dev.TenantOrgID,
		DeviceID:     dev.DeviceID,
		Content:      string(content),
		SHA256:       sha,
		Size:         len(content),
		PackageCount: len(snap.Packages),
		Model:        dev.Model,
		Source:       "manual",
		CapturedAt:   meta.CapturedAt.Format(time.RFC3339),
	})
	if serr != nil {
		a.Log.Warn("config snapshot store failed", "device_id", deviceID, "err", serr)
		redirectBack("", "快照入库失败: "+serr.Error())
		return
	}
	a.Log.Info("config snapshot captured",
		"device_id", deviceID, "snapshot_id", res.Snapshot.SnapshotID,
		"packages", len(snap.Packages), "size", len(content), "dedup", res.Deduplicated)
	if res.Deduplicated {
		redirectBack("配置未变化，已复用最近快照（"+shortID(res.Snapshot.SnapshotID)+"）", "")
		return
	}
	redirectBack(fmt.Sprintf("采集完成：%d 个配置包，%d 字节", len(snap.Packages), len(content)), "")
}

// handleConfigSnapshotDetail GET /devices/:id/config/snapshots/:sid 详情页。
func (a *App) handleConfigSnapshotDetail(c *gin.Context) {
	deviceID := c.Param("id")
	snapshotID := c.Param("sid")
	dev, err := a.Frps.GetDevice(c.Request.Context(), deviceID)
	if err != nil {
		c.String(http.StatusNotFound, "设备未找到：%s", deviceID)
		return
	}
	snap, serr := a.Frps.GetConfigSnapshot(c.Request.Context(), snapshotID)
	if serr != nil || snap.DeviceID != deviceID {
		c.String(http.StatusNotFound, "快照不存在：%s", snapshotID)
		return
	}

	var parsed configmgr.Snapshot
	pretty := snap.Content
	var rows []packageRow
	if jerr := json.Unmarshal([]byte(snap.Content), &parsed); jerr == nil {
		if ind, ierr := json.MarshalIndent(parsed, "", "  "); ierr == nil {
			pretty = string(ind)
		}
		for name, values := range parsed.Packages {
			rows = append(rows, packageRow{Name: name, Sections: len(values)})
		}
		sortPackageRows(rows)
	}

	u, _ := auth.CurrentUser(c)
	pg := configSnapshotDetailPage{
		pageBase:    pageBase{ActiveMenu: "devices", CurrentUser: u, Config: a.Cfg},
		Device:      dev,
		Snapshot:    snap,
		PrettyJSON:  pretty,
		PackageRows: rows,
		SourceLabel: snapshotSourceLabel,
	}
	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/devices/snapshot_detail", pg); rerr != nil {
		a.Log.Error("render snapshot detail failed", "err", rerr)
	}
}

// handleConfigSnapshotDownload GET /devices/:id/config/snapshots/:sid/download 下载 JSON。
func (a *App) handleConfigSnapshotDownload(c *gin.Context) {
	deviceID := c.Param("id")
	snapshotID := c.Param("sid")
	snap, serr := a.Frps.GetConfigSnapshot(c.Request.Context(), snapshotID)
	if serr != nil || snap.DeviceID != deviceID {
		c.String(http.StatusNotFound, "快照不存在：%s", snapshotID)
		return
	}
	filename := fmt.Sprintf("config-%s-%s.json", deviceID, snap.CapturedAt.Format("20060102-150405"))
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(snap.Content))
}

// handleConfigSnapshotDelete POST /devices/:id/config/snapshots/:sid/delete 删除快照。
func (a *App) handleConfigSnapshotDelete(c *gin.Context) {
	deviceID := c.Param("id")
	snapshotID := c.Param("sid")
	redirectBack := func(ok, errMsg string) {
		q := ""
		if ok != "" {
			q = "?ok=" + urlQueryEscape(ok)
		} else if errMsg != "" {
			q = "?err=" + urlQueryEscape(errMsg)
		}
		c.Redirect(http.StatusFound, "/devices/"+deviceID+"/config/snapshots"+q)
	}
	if derr := a.Frps.DeleteConfigSnapshot(c.Request.Context(), snapshotID); derr != nil {
		redirectBack("", "删除失败: "+derr.Error())
		return
	}
	a.Log.Info("config snapshot deleted", "device_id", deviceID, "snapshot_id", snapshotID)
	redirectBack("快照已删除", "")
}

// buildSnapshotPageLinks 构造快照列表分页条目（逻辑同 buildPageLinks，URL 由回调生成）。
func buildSnapshotPageLinks(page, total int, urlFor func(int) string) []pageLink {
	links := make([]pageLink, 0, 10)
	links = append(links, pageLink{Label: "«", URL: urlFor(page - 1), Disabled: page <= 1})
	start := page - 2
	if start < 1 {
		start = 1
	}
	end := page + 2
	if end > total {
		end = total
	}
	if start > 1 {
		links = append(links, pageLink{Label: "1", URL: urlFor(1)})
		if start > 2 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
	}
	for i := start; i <= end; i++ {
		links = append(links, pageLink{Label: strconv.Itoa(i), URL: urlFor(i), Active: i == page})
	}
	if end < total {
		if end < total-1 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
		links = append(links, pageLink{Label: strconv.Itoa(total), URL: urlFor(total)})
	}
	links = append(links, pageLink{Label: "»", URL: urlFor(page + 1), Disabled: page >= total})
	return links
}

// urlQueryEscape url.QueryEscape 的短别名。
func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}

// sortPackageRows 按包名排序详情页行。
func sortPackageRows(rows []packageRow) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
}

// shortID 取 UUID 前 8 位用于消息提示。
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
