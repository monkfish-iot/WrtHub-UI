package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
)

// usersPlaceholderPage 是用户管理占位页视图（功能待实现，见部署手册 §13）。
type usersPlaceholderPage struct {
	pageBase
}

// handleUsersPlaceholder 渲染"用户管理功能开发中"占位页。
// 现阶段无 UI；密码重置走命令行（部署手册 §8.2）或删库重建（§8.3）。
func (a *App) handleUsersPlaceholder(c *gin.Context) {
	u, _ := auth.CurrentUser(c)
	pg := usersPlaceholderPage{
		pageBase: pageBase{ActiveMenu: "users", CurrentUser: u, Config: a.Cfg},
	}
	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/users", pg); rerr != nil {
		a.Log.Error("render users placeholder failed", "err", rerr)
	}
}
