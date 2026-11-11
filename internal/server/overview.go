package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/frpsclient"
	"wrthub-ui/internal/store"
)

// overviewRecentLimit 概况页「最近日志」展示条数。
const overviewRecentLimit = 8

// radiusRuntimeView 是 RADIUS 服务运行时状态（由 frps_helper runtime map 提取为强类型，便于模板渲染）。
type radiusRuntimeView struct {
	Available   bool
	Running     bool
	AuthEnabled bool
	AcctEnabled bool
	AuthSource  string
	AuthAddr    string
	AcctAddr    string
}

// overviewPage 是主菜单「概况」页视图。
type overviewPage struct {
	pageBase
	UpdatedAt time.Time

	// 设备统计（frps_helper 实时查询；page_size=1 仅取 total）
	DeviceTotal      int
	DeviceOnline     int
	DeviceOffline    int
	DevicePending    int
	DeviceOnlinePct  int
	DeviceOfflinePct int
	DevicePendingPct int
	DevicesErr       string

	// 认证与在线统计
	SessionTotal      int
	SessionsErr       string
	LocalUserTotal    int
	LocalUserActive   int
	LocalUserDisabled int
	LocalUsersErr     string
	RadCfg            *frpsclient.RadiusGlobalConfig
	RadErr            string
	Runtime           radiusRuntimeView

	// PORTAL 今日认证
	AuthToday        int64
	AuthTodaySuccess int64
	AuthTodayFail    int64
	AuthErr          string

	// 最近日志
	RecentOps   []store.AuditLog
	RecentAuths []store.PortalAuthLog

	// 同机服务运行状态（frps / frps-helper / portal-server / WrtHub-UI）
	Services []serviceStatusView

	frpsStatus      *frpsclient.SystemStatus
	frpsStatusErr   error
	portalHealth    *portalHealth
	portalHealthErr error
}

// handleOverview 渲染概况页（统计卡片 + 状态分布 + 最近日志）。
// 各数据源相互独立，并发拉取；任一失败只影响对应卡片并给出提示，不阻断整页。
func (a *App) handleOverview(c *gin.Context) {
	ctx := c.Request.Context()
	u, _ := auth.CurrentUser(c)
	pg := overviewPage{
		pageBase:  pageBase{ActiveMenu: "overview", CurrentUser: u, Config: a.Cfg},
		UpdatedAt: time.Now(),
	}

	var wg sync.WaitGroup
	load := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}

	// 设备总数 + 三种状态计数
	load(func() {
		res, err := a.Frps.ListDevices(ctx, frpsclient.ListOptions{PageSize: 1})
		if err != nil {
			a.Log.Error("overview list devices failed", "err", err)
			pg.DevicesErr = err.Error()
			return
		}
		pg.DeviceTotal = res.Total
	})
	load(func() {
		res, err := a.Frps.ListDevices(ctx, frpsclient.ListOptions{PageSize: 1, Status: "online"})
		if err != nil {
			return
		}
		pg.DeviceOnline = res.Total
	})
	load(func() {
		res, err := a.Frps.ListDevices(ctx, frpsclient.ListOptions{PageSize: 1, Status: "offline"})
		if err != nil {
			return
		}
		pg.DeviceOffline = res.Total
	})
	load(func() {
		res, err := a.Frps.ListDevices(ctx, frpsclient.ListOptions{PageSize: 1, Status: "pending"})
		if err != nil {
			return
		}
		pg.DevicePending = res.Total
	})

	// 在线记账会话总数
	load(func() {
		res, err := a.Frps.ListRadiusSessions(ctx, 1, 1, "")
		if err != nil {
			a.Log.Error("overview list sessions failed", "err", err)
			pg.SessionsErr = err.Error()
			return
		}
		pg.SessionTotal = res.Total
	})

	// 本地 RADIUS 用户总数与启用数
	load(func() {
		res, err := a.Frps.ListLocalUsers(ctx, 1, 1, "", "")
		if err != nil {
			a.Log.Error("overview list local users failed", "err", err)
			pg.LocalUsersErr = err.Error()
			return
		}
		pg.LocalUserTotal = res.Total
		ar, aerr := a.Frps.ListLocalUsers(ctx, 1, 1, "", "active")
		if aerr != nil {
			return
		}
		pg.LocalUserActive = ar.Total
	})

	// RADIUS 全局配置与运行时状态
	load(func() {
		cfg, err := a.Frps.GetRadiusConfig(ctx)
		if err != nil {
			a.Log.Error("overview get radius config failed", "err", err)
			pg.RadErr = err.Error()
			return
		}
		pg.RadCfg = cfg
		pg.Runtime = parseRadiusRuntime(cfg)
	})

	// 今日 PORTAL 认证统计
	load(func() {
		now := time.Now()
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		base := a.DB.Model(&store.PortalAuthLog{}).Where("request_time >= ?", start)
		if err := base.Count(&pg.AuthToday).Error; err != nil {
			a.Log.Error("overview count today auth logs failed", "err", err)
			pg.AuthErr = err.Error()
			return
		}
		if err := a.DB.Model(&store.PortalAuthLog{}).
			Where("request_time >= ? AND status = ?", start, "success").
			Count(&pg.AuthTodaySuccess).Error; err != nil {
			a.Log.Error("overview count today success auth failed", "err", err)
		}
		if err := a.DB.Model(&store.PortalAuthLog{}).
			Where("request_time >= ? AND status = ?", start, "failed").
			Count(&pg.AuthTodayFail).Error; err != nil {
			a.Log.Error("overview count today failed auth failed", "err", err)
		}
	})

	// 最近操作日志与认证日志（本地 DB）
	load(func() {
		if err := a.DB.Order("created_at DESC").Limit(overviewRecentLimit).Find(&pg.RecentOps).Error; err != nil {
			a.Log.Error("overview list operation logs failed", "err", err)
		}
	})
	load(func() {
		if err := a.DB.Order("request_time DESC").Limit(overviewRecentLimit).Find(&pg.RecentAuths).Error; err != nil {
			a.Log.Error("overview list portal auth logs failed", "err", err)
		}
	})

	// frps_helper 运行状态（含 FRPS dashboard 聚合信息）
	load(func() {
		statusCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		st, err := a.Frps.GetSystemStatus(statusCtx)
		if err != nil {
			a.Log.Warn("overview get frps_helper status failed", "err", err)
		}
		pg.frpsStatus = st
		pg.frpsStatusErr = err
	})

	// portal-server 运行状态
	load(func() {
		h, err := fetchPortalHealth(ctx, a.Cfg.Services.PortalServer)
		if err != nil {
			a.Log.Warn("overview get portal-server health failed", "err", err)
		}
		pg.portalHealth = h
		pg.portalHealthErr = err
	})

	wg.Wait()

	// 服务运行状态：frps → frps-helper → portal-server → WrtHub-UI
	pg.Services = []serviceStatusView{
		serviceFrpsStatus(pg.frpsStatus, pg.frpsStatusErr),
		serviceFrpsHelperStatus(pg.frpsStatus, pg.frpsStatusErr),
		servicePortalStatus(pg.portalHealth, pg.portalHealthErr, a.Cfg.Services.PortalServer.BaseURL),
		a.selfServiceStatus(pg.UpdatedAt),
	}

	// 状态占比（用于分布进度条）
	if pg.DeviceTotal > 0 {
		pg.DeviceOnlinePct = pg.DeviceOnline * 100 / pg.DeviceTotal
		pg.DeviceOfflinePct = pg.DeviceOffline * 100 / pg.DeviceTotal
		pg.DevicePendingPct = pg.DevicePending * 100 / pg.DeviceTotal
	}
	if pg.LocalUserTotal >= pg.LocalUserActive {
		pg.LocalUserDisabled = pg.LocalUserTotal - pg.LocalUserActive
	}

	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/overview/index", pg); rerr != nil {
		a.Log.Error("render overview page failed", "err", rerr)
	}
}

// parseRadiusRuntime 将 frps_helper 返回的 runtime map 提取为强类型视图。
// runtime 缺失（RADIUS 控制器未安装）时 Available=false。
func parseRadiusRuntime(cfg *frpsclient.RadiusGlobalConfig) radiusRuntimeView {
	if cfg == nil || cfg.Runtime == nil {
		return radiusRuntimeView{}
	}
	v := radiusRuntimeView{Available: true}
	if b, ok := cfg.Runtime["running"].(bool); ok {
		v.Running = b
	}
	if b, ok := cfg.Runtime["auth_enabled"].(bool); ok {
		v.AuthEnabled = b
	}
	if b, ok := cfg.Runtime["acct_enabled"].(bool); ok {
		v.AcctEnabled = b
	}
	if s, ok := cfg.Runtime["auth_source"].(string); ok {
		v.AuthSource = s
	}
	if s, ok := cfg.Runtime["auth_addr"].(string); ok {
		v.AuthAddr = s
	}
	if s, ok := cfg.Runtime["acct_addr"].(string); ok {
		v.AcctAddr = s
	}
	return v
}
