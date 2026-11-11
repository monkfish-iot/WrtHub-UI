package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/frpsclient"
)

// ---------- 本地用户 CRUD 代理（浏览器 JS → WrtHub-UI → frps_helper） ----------
//
// 模板里的 fetch 不能直接打 frps_helper（管理接口可能只监听 127.0.0.1，
// 且 CORS 未配置），所以全部经由 WrtHub-UI 后端做代理，保持同域 + 权限守卫。

// handleLocalUsers 本地用户列表（GET /radius/api/local-users）。
func (a *App) handleLocalUsers(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	size := atoiDefault(c.Query("size"), 50)
	q := strings.TrimSpace(c.Query("q"))
	status := strings.TrimSpace(c.Query("status"))

	res, err := a.Frps.ListLocalUsers(c.Request.Context(), page, size, q, status)
	if err != nil {
		a.Log.Error("list local users failed", "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "ok",
		"data":    res,
	})
}

// handleLocalUserDetail 本地用户详情（GET /auth-source/api/local-users/:id），供编辑弹窗预填。
func (a *App) handleLocalUserDetail(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "invalid_id"})
		return
	}
	item, err := a.Frps.GetLocalUser(c.Request.Context(), uint(id))
	if err != nil {
		a.Log.Error("get local user detail failed", "id", id, "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": item})
}

// handleLocalUserCreate 新建本地用户（POST /radius/api/local-users）。
func (a *App) handleLocalUserCreate(c *gin.Context) {
	var req frpsclient.LocalUserCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "bad_json: " + err.Error()})
		return
	}
	item, err := a.Frps.CreateLocalUser(c.Request.Context(), &req)
	if err != nil {
		a.Log.Error("create local user failed", "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": item})
}

// handleLocalUserUpdate 更新本地用户（PUT /radius/api/local-users/:id）。
func (a *App) handleLocalUserUpdate(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "invalid_id"})
		return
	}
	var req frpsclient.LocalUserUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "bad_json: " + err.Error()})
		return
	}
	if err := a.Frps.UpdateLocalUser(c.Request.Context(), uint(id), &req); err != nil {
		a.Log.Error("update local user failed", "id", id, "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": nil})
}

// handleLocalUserDelete 删除本地用户（DELETE /radius/api/local-users/:id）。
func (a *App) handleLocalUserDelete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "invalid_id"})
		return
	}
	if err := a.Frps.DeleteLocalUser(c.Request.Context(), uint(id)); err != nil {
		a.Log.Error("delete local user failed", "id", id, "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": nil})
}

// handleLocalUserLogs 查询登录日志（GET /radius/api/local-users/:id/login-logs）。
func (a *App) handleLocalUserLogs(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "invalid_id"})
		return
	}
	limit := atoiDefault(c.Query("limit"), 50)
	res, err := a.Frps.ListLocalUserLogs(c.Request.Context(), uint(id), limit)
	if err != nil {
		a.Log.Error("list local user logs failed", "id", id, "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	// 与 frps_helper 原生信封对齐：data 下直接是 {username, logs}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": gin.H{
		"username": res.Username,
		"logs":     res.Logs,
	}})
}
