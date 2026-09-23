package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/store"
)

// 登录页内联 HTML（最小可用，待 §8 模板与 Bootstrap 本地化后迁移到 templates/login.html）。
// 错误通过 query err=1 控制，避免暴露用户存在性。
const loginHTMLTpl = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>WrtHub-UI 登录</title>
<style>
body{font-family:system-ui,Segoe UI,Roboto,Arial,sans-serif;background:#f5f6f8;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center}
.card{background:#fff;border:1px solid #e3e6eb;border-radius:8px;padding:28px 32px;width:340px;box-shadow:0 2px 8px rgba(0,0,0,.05)}
h1{margin:0 0 4px;font-size:20px}
p.sub{margin:0 0 18px;color:#6c757d;font-size:13px}
label{display:block;font-size:13px;margin:10px 0 4px}
input{width:100%;box-sizing:border-box;padding:8px 10px;border:1px solid #d0d5dd;border-radius:6px;font-size:14px}
button{width:100%;margin-top:18px;padding:9px;border:0;border-radius:6px;background:#0d6efd;color:#fff;font-size:14px;cursor:pointer}
.err{color:#d92d20;font-size:13px;margin:10px 0}
</style>
</head>
<body>
<form class="card" method="post" action="/login">
<h1>WrtHub-UI</h1>
<p class="sub">OpenWrt 设备管理平台</p>
{{ERR}}
<label for="u">用户名</label>
<input id="u" name="username" autofocus required>
<label for="p">密码</label>
<input id="p" type="password" name="password" required>
<button type="submit">登录</button>
</form>
</body>
</html>`

func loginPage(showErr bool) string {
	box := ""
	if showErr {
		box = `<div class="err">用户名或密码错误</div>`
	}
	return strings.Replace(loginHTMLTpl, "{{ERR}}", box, 1)
}

// registerAuthRoutes 注册公开的登录/登出路由（不受 RequireAuth 保护）。
// 登录成功/失败、登出均落审计日志（开发技术文档 §5.2）。
func registerAuthRoutes(r *gin.Engine, app *App) {
	r.GET("/login", func(c *gin.Context) {
		// 已登录直接跳首页，避免重复登录。
		if _, ok := auth.CurrentUserID(c); ok {
			c.Redirect(http.StatusFound, "/")
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(loginPage(c.Query("err") == "1")))
	})

	r.POST("/login", func(c *gin.Context) {
		username := c.PostForm("username")
		password := c.PostForm("password")
		user, err := app.Auth.Authenticate(c.Request.Context(), username, password)
		if err != nil {
			// 记录登录失败审计（不区分原因，便于事后排查暴力破解）
			_ = store.RecordAudit(app.DB, store.AuditEntry{
				Username:  username,
				Action:    "user.login_failed",
				Target:    username,
				IP:        c.ClientIP(),
				UserAgent: c.Request.UserAgent(),
			})
			// 统一返回 err=1，不区分用户不存在/密码错误/禁用，防枚举。
			c.Redirect(http.StatusFound, "/login?err=1")
			return
		}
		auth.Login(c, user)
		_ = store.RecordAudit(app.DB, store.AuditEntry{
			UserID:    user.ID,
			Username:  user.Username,
			Action:    "user.login",
			Target:    user.Username,
			IP:        c.ClientIP(),
			UserAgent: c.Request.UserAgent(),
		})
		c.Redirect(http.StatusFound, "/")
	})

	r.POST("/logout", func(c *gin.Context) {
		if uid, ok := auth.CurrentUserID(c); ok {
			uname, _ := auth.CurrentUsername(c)
			_ = store.RecordAudit(app.DB, store.AuditEntry{
				UserID:    uid,
				Username:  uname,
				Action:    "user.logout",
				IP:        c.ClientIP(),
				UserAgent: c.Request.UserAgent(),
			})
		}
		auth.Logout(c)
		c.Redirect(http.StatusFound, "/login")
	})
}
