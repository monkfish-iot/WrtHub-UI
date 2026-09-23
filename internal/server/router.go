package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
)

// Mount 注册全部路由。todo5/6 在此扩展设备/无线等业务路由。
func Mount(r *gin.Engine, app *App) {
	// 全局会话中间件：必须在所有使用 session 的路由前注册。
	r.Use(auth.SessionMiddleware(app.Cfg.Server.SessionSecret))

	// 静态资源：/assets/* 不受 RequireAuth 保护（登录页也需要 Bootstrap/htmx）。
	registerAssetsRoutes(r, app)

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 公开路由：登录/登出（不受 RequireAuth 保护）。
	registerAuthRoutes(r, app)

	// 受保护路由组：未登录将被 RequireAuth 拒绝（HTML→/login、HTMX→401+HX-Redirect、JSON→401）。
	protected := r.Group("/", auth.RequireAuth(app.DB))
	protected.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/devices")
	})
	protected.GET("/devices", auth.RequirePermission(app.DB, "device:read"), app.handleDevicesList)
	protected.GET("/devices/:id", auth.RequirePermission(app.DB, "device:read"), app.handleDeviceDetail)
	protected.GET("/devices/:id/wireless", auth.RequirePermission(app.DB, "wireless:read"), app.handleWireless)
	protected.GET("/devices/:id/config", auth.RequirePermission(app.DB, "device:read"), app.handleDeviceConfig)
	// 无线网卡参数编辑（uci.set + uci.apply rollback，见 无线配置功能开发文档.md §5.3）。
	protected.POST("/devices/:id/config/wireless/radio", auth.RequirePermission(app.DB, "wireless:write"), app.handleDeviceConfigRadioEdit)
	// SSID 新增/编辑（section 空 = uci.add，否则 uci.set）与删除（uci.delete），见 §5.3 / §3.4 抓包参考。
	protected.POST("/devices/:id/config/wireless/ssid", auth.RequirePermission(app.DB, "wireless:write"), app.handleDeviceConfigSSIDEdit)
	protected.POST("/devices/:id/config/wireless/ssid/delete", auth.RequirePermission(app.DB, "wireless:write"), app.handleDeviceConfigSSIDDelete)
	protected.DELETE("/devices/:id/config/wireless/ssid", auth.RequirePermission(app.DB, "wireless:write"), app.handleDeviceConfigSSIDDelete)
	// /wireless 顶级入口：未指定设备时跳到设备列表，由用户从列表选设备进入无线管理。
	protected.GET("/wireless", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/devices")
	})
	// /users 占位（用户管理 UI 待实现，见部署手册 §13；现阶段先避免菜单点击 404）。
	protected.GET("/users", auth.RequirePermission(app.DB, "user:read"), app.handleUsersPlaceholder)
	protected.GET("/audit", auth.RequirePermission(app.DB, "audit:read"), app.handleAuditList)
}
