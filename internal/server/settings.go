package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/firmware"
	"wrthub-ui/internal/frpsclient"
	"wrthub-ui/internal/store"
)

// settingsInt 解析系统设置表单的整数字段（空串取默认值）。
func settingsInt(raw string, def, min, max int, label string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		return 0, fmt.Errorf("%s须为 %d-%d 的整数", label, min, max)
	}
	return v, nil
}

// settingsPage 是「系统设置」页面视图。
type settingsPage struct {
	pageBase
	S         *store.Setting
	RadCfg    *frpsclient.RadiusGlobalConfig
	RadCfgErr string
	// 证书管理 TAB（原独立页面并入系统设置）
	Tab        string
	CanCert    bool
	CertCards  []certCard
	CertCfgErr string
	// 固件管理 TAB：上传固件并自动生成说明 JSON，按型号分组展示全部固件。
	CanFirmware   bool
	FirmwareRoot  string
	FirmwareMaxMB int64
	FirmwareErr   string
	FirmwareList  []firmware.ModelGroup
	FlashOK       string
	FlashErr      string
}

// handleSettingsPage 渲染系统设置页（值取自 settings 表，DB 覆盖层）。
// 同时加载 RADIUS 服务配置（RADIUS 服务 TAB）、证书槽位（证书管理 TAB）
// 与磁盘固件库（固件管理 TAB）。
func (a *App) handleSettingsPage(c *gin.Context) {
	u, _ := auth.CurrentUser(c)
	pg := settingsPage{
		pageBase: pageBase{ActiveMenu: "settings", CurrentUser: u, Config: a.Cfg},
		Tab:      c.Query("tab"),
		FlashOK:  c.Query("ok"),
		FlashErr: c.Query("err"),
	}
	s, err := store.LoadSettings(a.DB)
	if err != nil {
		a.Log.Error("load settings failed", "err", err)
		pg.FlashErr = "加载设置失败：" + err.Error()
	} else if s == nil {
		// 表被清空后访问页面：按当前文件配置即时种子，保证页面有值
		if _, serr := store.SeedSettings(a.DB, a.Cfg); serr != nil {
			a.Log.Error("re-seed settings failed", "err", serr)
			pg.FlashErr = "设置表为空且种子失败：" + serr.Error()
		} else {
			s, _ = store.LoadSettings(a.DB)
		}
	}
	pg.S = s

	// 加载 RADIUS 服务配置（用于「RADIUS 服务」TAB）
	cfg, rerr := a.Frps.GetRadiusConfig(c.Request.Context())
	if rerr != nil {
		a.Log.Error("get radius config failed", "err", rerr)
		pg.RadCfgErr = "获取全局 RADIUS 配置失败：" + rerr.Error()
	} else {
		pg.RadCfg = radiusDisplayCfg(cfg)
	}

	// 证书管理 TAB：数据来自 frps_helper 证书接口；可见性按 cert:read 判定，
	// 写操作仍由证书 API 路由上的 cert:write 权限约束。
	pg.CanCert = a.canAccessPermission(u, "cert:read")
	if pg.CanCert {
		items, cerr := a.Frps.ListCertificates(c.Request.Context())
		if cerr != nil {
			a.Log.Error("list certificates failed", "err", cerr)
			pg.CertCfgErr = "获取证书列表失败：" + cerr.Error()
		} else {
			pg.CertCards = certCards(items)
		}
	}

	// 固件管理 TAB：直接读取磁盘固件库；可见性按 firmware:read 判定，
	// 上传/备注/删除仍由对应路由上的 firmware:write 约束。
	pg.CanFirmware = a.canAccessPermission(u, "firmware:read")
	if pg.CanFirmware {
		pg.FirmwareRoot = a.Firmware.Root()
		pg.FirmwareMaxMB = a.Cfg.Firmware.MaxSizeMB
		groups, ferr := a.Firmware.ListAll()
		if ferr != nil {
			a.Log.Error("list firmware failed", "err", ferr)
			pg.FirmwareErr = "读取固件库失败：" + ferr.Error()
		} else {
			pg.FirmwareList = groups
		}
	}

	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/settings", pg); rerr != nil {
		a.Log.Error("render settings page failed", "err", rerr)
	}
}

// canAccessPermission 判断当前用户是否拥有权限（admin 角色直接放行）。
func (a *App) canAccessPermission(u *store.User, code string) bool {
	if u == nil {
		return false
	}
	if isAdmin, err := store.IsAdmin(a.DB, u.ID); err == nil && isAdmin {
		return true
	}
	ok, err := store.HasPermission(a.DB, u.ID, code)
	return err == nil && ok
}

// 系统设置表单校验范围（秒）
const (
	settingTimeoutMinSec   = 1
	settingTimeoutMaxSec   = 300  // 普通请求超时上限
	settingUploadMinSec    = 60   // 固件上传至少 1 分钟
	settingUploadMaxSec    = 7200 // 固件上传上限 2 小时
	settingTokenTTLMinSec  = 30
	settingTokenTTLMaxSec  = 86400
	settingTemplateRootMax = 200
)

// handleSettingsUpdate 保存系统设置（POST /settings）：
// 持久化到 settings 表后热生效——超时类作用于后续请求，模板根目录立即切换（原目录模板文件不迁移）。
func (a *App) handleSettingsUpdate(c *gin.Context) {
	back := "/settings"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+urlQuery(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+urlQuery(msg)) }

	templateRoot := strings.TrimSpace(c.PostForm("portal_template_root"))
	if templateRoot == "" || len(templateRoot) > settingTemplateRootMax {
		errMsg("PORTAL 模板根目录不能为空且不超过 200 字符")
		return
	}
	frpsTimeout, err := settingsInt(c.PostForm("frps_request_timeout"), 10, settingTimeoutMinSec, settingTimeoutMaxSec, "frps_helper 请求超时")
	if err != nil {
		errMsg(err.Error())
		return
	}
	rpcTimeout, err := settingsInt(c.PostForm("device_rpc_timeout"), 10, settingTimeoutMinSec, settingTimeoutMaxSec, "设备 RPC 超时")
	if err != nil {
		errMsg(err.Error())
		return
	}
	uploadTimeout, err := settingsInt(c.PostForm("device_rpc_upload_timeout"), 1800, settingUploadMinSec, settingUploadMaxSec, "固件上传超时")
	if err != nil {
		errMsg(err.Error())
		return
	}
	tokenTTL, err := settingsInt(c.PostForm("device_rpc_token_cache_ttl"), 300, settingTokenTTLMinSec, settingTokenTTLMaxSec, "凭证缓存 TTL")
	if err != nil {
		errMsg(err.Error())
		return
	}

	s, err := store.LoadSettings(a.DB)
	if err != nil {
		a.Log.Error("load settings failed", "err", err)
		errMsg("读取设置失败：" + err.Error())
		return
	}
	if s == nil {
		s = store.SettingFromConfig(a.Cfg)
	}
	s.PortalTemplateRoot = templateRoot
	s.FrpsRequestTimeoutSec = frpsTimeout
	s.DeviceRpcTimeoutSec = rpcTimeout
	s.DeviceRpcUploadTimeoutSec = uploadTimeout
	s.DeviceRpcTokenCacheTTLSec = tokenTTL

	if err := store.SaveSettings(a.DB, s); err != nil {
		a.Log.Error("save settings failed", "err", err)
		errMsg("保存失败：" + err.Error())
		return
	}

	// 热生效：超时类作用于后续请求；模板根目录立即切换（目录不存在时自动创建）。
	rpcTimeoutDur := time.Duration(rpcTimeout) * time.Second
	tokenTTLDur := time.Duration(tokenTTL) * time.Second
	a.Frps.SetTimeout(time.Duration(frpsTimeout) * time.Second)
	a.RPC.SetTimeout(rpcTimeoutDur)
	a.RPC.SetTokenCacheTTL(tokenTTLDur)
	a.Ubus.SetTimeouts(rpcTimeoutDur, time.Duration(uploadTimeout)*time.Second)
	a.CredProv.SetCredTTL(tokenTTLDur)
	if err := a.PortalStore.SetRoot(templateRoot); err != nil {
		a.Log.Error("apply portal template root failed", "root", templateRoot, "err", err)
		errMsg("设置已保存，但模板根目录应用失败：" + err.Error())
		return
	}
	// 同步内存配置（本进程内后续读取与重启前展示一致；文件默认层不回写）
	s.ApplyToConfig(a.Cfg)

	a.Log.Info("settings updated", "operator", currentName(c),
		"template_root", templateRoot,
		"frps_timeout_s", frpsTimeout, "rpc_timeout_s", rpcTimeout,
		"upload_timeout_s", uploadTimeout, "token_ttl_s", tokenTTL)
	okMsg("系统设置已保存并生效")
}

// firmwareNotesMax 固件备注最大字符数（按 Unicode 字符计数）。
const firmwareNotesMax = 500

// handleFirmwareUpload 上传固件（POST /settings/firmware/upload，multipart）。
// 文件落到 firmware/<型号目录>/，并自动生成同名说明 JSON（version/日期/校验和/型号）。
func (a *App) handleFirmwareUpload(c *gin.Context) {
	back := "/settings?tab=firmware"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&ok="+urlQuery(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&err="+urlQuery(msg)) }

	maxMB := a.Cfg.Firmware.MaxSizeMB
	if maxMB <= 0 {
		maxMB = 512
	}
	maxBytes := maxMB * 1024 * 1024
	// multipart 除文件外还有表单项与分隔符，额外留 2MB 余量。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+2*1024*1024)

	model := strings.TrimSpace(c.PostForm("model"))
	notes := strings.TrimSpace(c.PostForm("notes"))
	if model == "" {
		errMsg("上传失败：硬件型号不能为空")
		return
	}
	if utf8.RuneCountInString(notes) > firmwareNotesMax {
		errMsg(fmt.Sprintf("上传失败：备注不能超过 %d 个字符", firmwareNotesMax))
		return
	}

	fh, err := c.FormFile("file")
	if err != nil {
		errMsg(fmt.Sprintf("上传失败：请选择固件文件（单文件上限 %d MB）", maxMB))
		return
	}
	filename := filepath.Base(fh.Filename)
	if !firmware.ValidFilename(filename) {
		errMsg("上传失败：固件文件名非法（仅允许字母、数字、点、下划线和短横线，且不能含路径）")
		return
	}
	if strings.HasSuffix(strings.ToLower(filename), ".json") {
		errMsg("上传失败：不能上传 .json 说明文件")
		return
	}
	if fh.Size <= 0 {
		errMsg("上传失败：固件文件为空")
		return
	}
	if fh.Size > maxBytes {
		errMsg(fmt.Sprintf("上传失败：固件文件超过大小上限（%d MB）", maxMB))
		return
	}
	f, err := fh.Open()
	if err != nil {
		errMsg("上传失败：读取上传文件失败")
		return
	}
	defer f.Close()

	desc, err := a.Firmware.Save(model, filename, notes, f, fh.Size)
	if err != nil {
		a.Log.Warn("firmware upload failed", "operator", currentName(c), "model", model, "filename", filename, "err", err)
		errMsg("上传失败：" + err.Error())
		return
	}
	a.Log.Info("firmware uploaded", "operator", currentName(c),
		"model", model, "filename", filename, "version", desc.Version, "size", fh.Size)
	okMsg(fmt.Sprintf("固件已上传：%s（型号 %s，版本 %s），说明 JSON 已自动生成", filename, model, desc.Version))
}

// handleFirmwareNotes 更新固件备注并重新生成说明 JSON（POST /settings/firmware/notes）。
func (a *App) handleFirmwareNotes(c *gin.Context) {
	back := "/settings?tab=firmware"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&ok="+urlQuery(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&err="+urlQuery(msg)) }

	model := strings.TrimSpace(c.PostForm("model"))
	filename := strings.TrimSpace(c.PostForm("filename"))
	notes := strings.TrimSpace(c.PostForm("notes"))
	if model == "" || filename == "" {
		errMsg("更新失败：缺少固件信息")
		return
	}
	if utf8.RuneCountInString(notes) > firmwareNotesMax {
		errMsg(fmt.Sprintf("更新失败：备注不能超过 %d 个字符", firmwareNotesMax))
		return
	}
	desc, err := a.Firmware.UpdateNotes(model, filename, notes)
	if err != nil {
		a.Log.Warn("firmware notes update failed", "operator", currentName(c), "model", model, "filename", filename, "err", err)
		errMsg("更新失败：" + err.Error())
		return
	}
	a.Log.Info("firmware notes updated", "operator", currentName(c),
		"model", model, "filename", filename, "version", desc.Version)
	okMsg(fmt.Sprintf("已更新 %s 的备注（版本 %s）", filename, desc.Version))
}

// handleFirmwareDelete 删除固件及同名说明 JSON（POST /settings/firmware/delete）。
func (a *App) handleFirmwareDelete(c *gin.Context) {
	back := "/settings?tab=firmware"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&ok="+urlQuery(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"&err="+urlQuery(msg)) }

	model := strings.TrimSpace(c.PostForm("model"))
	filename := strings.TrimSpace(c.PostForm("filename"))
	if model == "" || filename == "" {
		errMsg("删除失败：缺少固件信息")
		return
	}
	if err := a.Firmware.Delete(model, filename); err != nil {
		a.Log.Warn("firmware delete failed", "operator", currentName(c), "model", model, "filename", filename, "err", err)
		errMsg("删除失败：" + err.Error())
		return
	}
	a.Log.Info("firmware deleted", "operator", currentName(c), "model", model, "filename", filename)
	okMsg("已删除固件 " + filename)
}

// handleFirmwareDownload 下载固件文件（GET /settings/firmware/download）。
func (a *App) handleFirmwareDownload(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	filename := strings.TrimSpace(c.Query("filename"))
	if model == "" || filename == "" {
		c.String(http.StatusBadRequest, "缺少固件参数")
		return
	}
	f, fi, _, err := a.Firmware.OpenFile(model, filename)
	if err != nil {
		c.String(http.StatusNotFound, "固件不存在或不可下载：%s", err.Error())
		return
	}
	defer f.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, filename, fi.ModTime(), f)
}

// handleFirmwareModelsAPI 返回上传下拉可选型号：固件库已有目录 + 已注册设备型号。
// 前端在展开「上传固件」时按需加载，避免系统设置页每次打开都多打一轮设备接口。
func (a *App) handleFirmwareModelsAPI(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"models": a.listFirmwareModels(c.Request.Context())})
}

// listFirmwareModels 汇总固件库目录与已注册设备型号（去重、排序）。
// 设备列表分页拉取，最多 5 页，避免设备数量很大时拖慢页面。
func (a *App) listFirmwareModels(ctx context.Context) []string {
	set := map[string]struct{}{}
	if groups, err := a.Firmware.ListAll(); err == nil {
		for _, g := range groups {
			name := strings.TrimSpace(g.Model)
			if name == "" {
				name = g.ModelDir
			}
			set[name] = struct{}{}
		}
	}
	res, err := a.Frps.ListDevices(ctx, frpsclient.ListOptions{Page: 1, PageSize: 100})
	if err != nil {
		a.Log.Warn("firmware: list device models failed", "err", err)
	} else {
		for _, d := range res.Items {
			if m := strings.TrimSpace(d.Model); m != "" {
				set[m] = struct{}{}
			}
		}
		// 追加分页（上限 5 页 / 500 台设备）
		for page := 2; page <= 5 && (page-1)*100 < res.Total; page++ {
			r, perr := a.Frps.ListDevices(ctx, frpsclient.ListOptions{Page: page, PageSize: 100})
			if perr != nil {
				a.Log.Warn("firmware: list device models page failed", "page", page, "err", perr)
				break
			}
			for _, d := range r.Items {
				if m := strings.TrimSpace(d.Model); m != "" {
					set[m] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
