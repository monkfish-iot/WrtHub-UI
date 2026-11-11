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
	// 全局操作日志中间件：拦截所有写操作（POST/PUT/DELETE/PATCH）统一落库。
	// 必须在 SessionMiddleware 之后挂载（依赖会话读取操作人身份）。
	r.Use(oplogMiddleware(app))

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
		c.Redirect(http.StatusFound, "/overview")
	})
	// 概况（默认首页）：设备/会话/认证统计与最近日志。
	protected.GET("/overview", auth.RequirePermission(app.DB, "device:read"), app.handleOverview)
	protected.GET("/devices", auth.RequirePermission(app.DB, "device:read"), app.handleDevicesList)
	protected.GET("/devices/:id", auth.RequirePermission(app.DB, "device:read"), app.handleDeviceDetail)
	protected.GET("/devices/:id/wireless", auth.RequirePermission(app.DB, "wireless:read"), app.handleWireless)
	protected.GET("/devices/:id/config", auth.RequirePermission(app.DB, "device:read"), app.handleDeviceConfig)
	// 重启设备（system reboot），写操作需 device:write。
	protected.POST("/devices/:id/config/reboot", auth.RequirePermission(app.DB, "device:write"), app.handleDeviceReboot)
	// 踢无线客户端下线（hostapd del_client），写操作需 wireless:write。
	protected.POST("/devices/:id/config/clients/kick", auth.RequirePermission(app.DB, "wireless:write"), app.handleDeviceKickClient)
	// 固件升级（cgi-io 上传 + sysupgrade），写操作需 device:write。
	protected.POST("/devices/:id/config/firmware/upgrade", auth.RequirePermission(app.DB, "device:write"), app.handleDeviceFirmwareUpgrade)
	// 无线网卡参数编辑（uci.set + uci.apply rollback，见 无线配置功能开发文档.md §5.3）。
	protected.POST("/devices/:id/config/wireless/radio", auth.RequirePermission(app.DB, "wireless:write"), app.handleDeviceConfigRadioEdit)
	// SSID 新增/编辑（section 空 = uci.add，否则 uci.set）与删除（uci.delete），见 §5.3 / §3.4 抓包参考。
	protected.POST("/devices/:id/config/wireless/ssid", auth.RequirePermission(app.DB, "wireless:write"), app.handleDeviceConfigSSIDEdit)
	protected.POST("/devices/:id/config/wireless/ssid/delete", auth.RequirePermission(app.DB, "wireless:write"), app.handleDeviceConfigSSIDDelete)
	protected.DELETE("/devices/:id/config/wireless/ssid", auth.RequirePermission(app.DB, "wireless:write"), app.handleDeviceConfigSSIDDelete)
	// 配置快照（P1）：采集为只读 uci 读取但属管理动作需 device:write；列表/详情/下载只需 device:read。
	protected.GET("/devices/:id/config/snapshots", auth.RequirePermission(app.DB, "device:read"), app.handleConfigSnapshots)
	protected.POST("/devices/:id/config/snapshots/capture", auth.RequirePermission(app.DB, "device:write"), app.handleConfigSnapshotCapture)
	protected.GET("/devices/:id/config/snapshots/:sid", auth.RequirePermission(app.DB, "device:read"), app.handleConfigSnapshotDetail)
	protected.GET("/devices/:id/config/snapshots/:sid/download", auth.RequirePermission(app.DB, "device:read"), app.handleConfigSnapshotDownload)
	protected.POST("/devices/:id/config/snapshots/:sid/delete", auth.RequirePermission(app.DB, "device:write"), app.handleConfigSnapshotDelete)
	// /wireless 顶级入口：未指定设备时跳到设备列表，由用户从列表选设备进入无线管理。
	protected.GET("/wireless", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/devices")
	})
	// 认证源管理：北向 Access-Request 的认证数据源（/auth-source）。
	protected.GET("/auth-source", auth.RequirePermission(app.DB, "device:read"), app.handleRadiusPage)
	protected.POST("/auth-source", auth.RequirePermission(app.DB, "device:write"), app.handleRadiusGlobalUpdate)
	// 旧路由兼容：/radius 已更名为 /auth-source。
	protected.GET("/radius", auth.RequirePermission(app.DB, "device:read"), func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/auth-source")
	})
	// RADIUS 服务配置已整合到「系统设置」页的 RADIUS 服务 TAB；旧 GET 路由重定向到 /settings。
	// POST /radius/service 保留用于表单提交（提交后重定向回 /settings）。
	protected.GET("/radius/service", auth.RequirePermission(app.DB, "device:read"), func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/settings")
	})
	protected.POST("/radius/service", auth.RequirePermission(app.DB, "device:write"), app.handleRadiusServiceUpdate)
	// 在线会话（主菜单，只读 + 5 秒自动刷新）
	protected.GET("/sessions", auth.RequirePermission(app.DB, "device:read"), app.handleSessionsPage)
	protected.GET("/sessions/data", auth.RequirePermission(app.DB, "device:read"), app.handleSessionsData)
	// 强制下线在线会话（经隧道调设备 ubus webauthd.session/drop_user），写操作需 device:write。
	protected.POST("/sessions/drop", auth.RequirePermission(app.DB, "device:write"), app.handleSessionDrop)
	// 每设备 RADIUS 对接参数（设备详情页维护）。
	protected.POST("/devices/:id/radius", auth.RequirePermission(app.DB, "device:write"), app.handleRadiusDeviceUpdate)
	// 设备认证源切换 + 对应参数保存（写设备 UCI webauthd，南向，与服务器认证源无关）。
	protected.POST("/auth-source/device/:id", auth.RequirePermission(app.DB, "device:write"), app.handleAuthSourceUpdate)
	// 本地 RADIUS 用户代理 API（浏览器 JS 经 WrtHub-UI 后端代理到 frps_helper 管理接口）。
	protected.GET("/auth-source/api/local-users", auth.RequirePermission(app.DB, "device:read"), app.handleLocalUsers)
	protected.GET("/auth-source/api/local-users/:id", auth.RequirePermission(app.DB, "device:read"), app.handleLocalUserDetail)
	protected.POST("/auth-source/api/local-users", auth.RequirePermission(app.DB, "device:write"), app.handleLocalUserCreate)
	protected.PUT("/auth-source/api/local-users/:id", auth.RequirePermission(app.DB, "device:write"), app.handleLocalUserUpdate)
	protected.DELETE("/auth-source/api/local-users/:id", auth.RequirePermission(app.DB, "device:write"), app.handleLocalUserDelete)
	protected.GET("/auth-source/api/local-users/:id/login-logs", auth.RequirePermission(app.DB, "device:read"), app.handleLocalUserLogs)
	// 设备预注册（对应 tentant-mng 绑定设备的单租户化）。
	protected.POST("/devices/pre-register", auth.RequirePermission(app.DB, "device:write"), app.handleDevicePreRegister)
	// 设备自动注册开关（与预注册互斥：开启时 UI 隐藏预注册入口）。
	protected.POST("/devices/auto-register", auth.RequirePermission(app.DB, "device:write"), app.handleDeviceAutoRegister)
	// 删除设备（frps_helper 软删除；在线设备由服务端拒绝）。
	protected.POST("/devices/delete", auth.RequirePermission(app.DB, "device:write"), app.handleDeviceDelete)
	// PORTAL 管理：认证模板 / 认证策略 / 认证日志（单租户化移植自 tentant-mng）。
	protected.GET("/portal/templates", auth.RequirePermission(app.DB, "portal:read"), app.handlePortalTemplates)
	protected.POST("/portal/templates", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalTemplateUpload)
	protected.POST("/portal/templates/:id/toggle", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalTemplateToggle)
	protected.POST("/portal/templates/:id/delete", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalTemplateDelete)
	protected.POST("/portal/templates/:id/fork", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalTemplateFork)
	protected.GET("/portal/templates/:id/files", auth.RequirePermission(app.DB, "portal:read"), app.handlePortalTemplateFiles)
	protected.GET("/portal/templates/:id/preview/*filepath", auth.RequirePermission(app.DB, "portal:read"), app.handlePortalTemplatePreview)
	// 模板文件在线编辑：编辑页（read）与 保存/新建/删除（write）。
	protected.GET("/portal/templates/:id/edit", auth.RequirePermission(app.DB, "portal:read"), app.handlePortalTemplateEdit)
	protected.POST("/portal/templates/:id/files/save", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalTemplateFileSave)
	protected.POST("/portal/templates/:id/files/create", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalTemplateFileCreate)
	protected.POST("/portal/templates/:id/files/delete", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalTemplateFileDelete)
	// 可视化编辑配置保存（schema.json 声明的属性）。
	protected.POST("/portal/templates/:id/config", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalTemplateConfigSave)
	protected.GET("/portal/policies", auth.RequirePermission(app.DB, "portal:read"), app.handlePortalPolicies)
	protected.POST("/portal/policies", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalPolicySave)
	protected.POST("/portal/policies/:id/toggle", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalPolicyToggle)
	protected.POST("/portal/policies/:id/delete", auth.RequirePermission(app.DB, "portal:write"), app.handlePortalPolicyDelete)
	protected.GET("/portal/auth-logs", auth.RequirePermission(app.DB, "portal:read"), app.handlePortalAuthLogs)
	protected.GET("/portal/auth-logs/:id", auth.RequirePermission(app.DB, "portal:read"), app.handlePortalAuthLogDetail)
	// /portal 顶级入口：默认跳到认证模板。
	protected.GET("/portal", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/portal/templates")
	})
	// 本地账号（平台用户）管理：列表/新建/编辑/启停/删除，登录日志复用操作日志。
	protected.GET("/users", auth.RequirePermission(app.DB, "user:read"), app.handleUsersList)
	protected.POST("/users", auth.RequirePermission(app.DB, "user:write"), app.handleUserCreate)
	protected.POST("/users/:id", auth.RequirePermission(app.DB, "user:write"), app.handleUserUpdate)
	protected.POST("/users/:id/toggle", auth.RequirePermission(app.DB, "user:write"), app.handleUserToggle)
	protected.POST("/users/:id/delete", auth.RequirePermission(app.DB, "user:write"), app.handleUserDelete)
	protected.GET("/operation-logs", auth.RequirePermission(app.DB, "audit:read"), app.handleAuditList)
	// 系统设置（业务配置 DB 管理：认证模式 / PORTAL 模板根目录 / 超时类，保存即热生效）。
	protected.GET("/settings", auth.RequirePermission(app.DB, "setting:read"), app.handleSettingsPage)
	protected.POST("/settings", auth.RequirePermission(app.DB, "setting:write"), app.handleSettingsUpdate)
	// 证书管理：frps_helper 三槽位（ca / server-auth / https）内容直传 + 热生效。
	// 页面入口为「系统设置」页的证书管理 TAB（/settings?tab=certs）；
	// 浏览器只访问本服务，由后端代理到 frps_helper 北向管理接口。
	protected.GET("/certificates/api/purposes/:purpose", auth.RequirePermission(app.DB, "cert:read"), app.handleCertDetailAPI)
	protected.POST("/certificates/api/purposes/:purpose/validate", auth.RequirePermission(app.DB, "cert:read"), app.handleCertValidateAPI)
	protected.GET("/certificates/api/purposes/:purpose/versions", auth.RequirePermission(app.DB, "cert:read"), app.handleCertVersionsAPI)
	protected.PUT("/certificates/api/purposes/:purpose", auth.RequirePermission(app.DB, "cert:write"), app.handleCertPutAPI)
	protected.DELETE("/certificates/api/purposes/:purpose", auth.RequirePermission(app.DB, "cert:write"), app.handleCertDeleteAPI)
	protected.POST("/certificates/api/purposes/:purpose/apply", auth.RequirePermission(app.DB, "cert:write"), app.handleCertApplyAPI)
	protected.POST("/certificates/api/purposes/:purpose/generate", auth.RequirePermission(app.DB, "cert:write"), app.handleCertGenerateAPI)
	protected.POST("/certificates/api/purposes/:purpose/rollback", auth.RequirePermission(app.DB, "cert:write"), app.handleCertRollbackAPI)
	protected.DELETE("/certificates/api/previous-ca", auth.RequirePermission(app.DB, "cert:write"), app.handleCertClearPreviousAPI)
	// 证书备份：变更前自动生成，列表/删除/恢复；每槽位最新一条只允许恢复。
	protected.GET("/certificates/api/backups", auth.RequirePermission(app.DB, "cert:read"), app.handleCertBackupsAPI)
	protected.DELETE("/certificates/api/backups/:id", auth.RequirePermission(app.DB, "cert:write"), app.handleCertBackupDeleteAPI)
	protected.POST("/certificates/api/backups/:id/restore", auth.RequirePermission(app.DB, "cert:write"), app.handleCertBackupRestoreAPI)
	// 固件管理：系统设置页的「固件管理」TAB，上传后自动生成说明 JSON。
	// 上传/备注/删除为写操作需 firmware:write；列表已随系统设置页加载，型号下拉与下载只需 firmware:read。
	protected.GET("/settings/firmware/models", auth.RequirePermission(app.DB, "firmware:read"), app.handleFirmwareModelsAPI)
	protected.GET("/settings/firmware/download", auth.RequirePermission(app.DB, "firmware:read"), app.handleFirmwareDownload)
	protected.POST("/settings/firmware/upload", auth.RequirePermission(app.DB, "firmware:write"), app.handleFirmwareUpload)
	protected.POST("/settings/firmware/notes", auth.RequirePermission(app.DB, "firmware:write"), app.handleFirmwareNotes)
	protected.POST("/settings/firmware/delete", auth.RequirePermission(app.DB, "firmware:write"), app.handleFirmwareDelete)
}
