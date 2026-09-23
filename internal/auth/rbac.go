package auth

import (
	"net/http"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"wrthub-ui/internal/store"
)

// CurrentUsername 从会话取用户名；未登录返回 "",false。
func CurrentUsername(c *gin.Context) (string, bool) {
	v := sessions.Default(c).Get(sessionUNameKey)
	if v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// RequirePermission 要求当前用户拥有指定权限 code；admin 角色绕过。
// 应在 RequireAuth 之后链式使用（依赖其注入的 currentUser）。
// 拒绝时返回 403（HTMX/JSON/HTML 三态）。
func RequirePermission(db *gorm.DB, code string) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := CurrentUser(c)
		if !ok {
			deny(c) // 未登录（RequireAuth 应已拦截，此处兜底）
			return
		}
		isAdmin, err := store.IsAdmin(db, u.ID)
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		if isAdmin {
			c.Next()
			return
		}
		ok, err = store.HasPermission(db, u.ID, code)
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		if !ok {
			denyForbidden(c, code)
			return
		}
		c.Next()
	}
}

func denyForbidden(c *gin.Context, code string) {
	msg := "403 Forbidden: 缺少权限 " + code
	if wantsJSON(c) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden", "permission": code})
		return
	}
	c.String(http.StatusForbidden, msg)
	c.Abort()
}
