// Package auth 提供认证抽象与会话管理。
//
// 设计目标（见开发技术文档 §5.3）：
//   - 抽象 Authenticator 接口，本地实现先行，后续可替换为 CASDOOR；
//   - 会话用 gin-contrib/sessions（cookie store），secret 来自 config.server.session_secret；
//   - 中间件 RequireAuth 拒绝未登录访问，区分 HTML / HTMX / JSON 三种响应方式。
//
// 权限校验（RBAC）在 todo7 实现，本包暂不做。
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"wrthub-ui/internal/store"
)

const (
	sessionName    = "wrthub"
	ctxUserKey     = "currentUser"
	sessionUIDKey  = "uid"
	sessionUNameKey = "uname"
)

// 预定义认证错误，避免泄露用户存在性（登录失败统一返回 ErrInvalidCredential）。
var (
	ErrInvalidCredential = errors.New("invalid username or password")
	ErrUserDisabled      = errors.New("user is disabled")
)

// Authenticator 抽象认证来源。Name 返回 "local" / "casdoor" 等。
type Authenticator interface {
	Name() string
	Authenticate(ctx context.Context, username, password string) (*store.User, error)
}

// LocalAuthenticator 基于 store.User 的本地账号密码认证。
type LocalAuthenticator struct {
	db  *gorm.DB
	log *slog.Logger
}

func NewLocalAuthenticator(db *gorm.DB, log *slog.Logger) *LocalAuthenticator {
	return &LocalAuthenticator{db: db, log: log}
}

func (a *LocalAuthenticator) Name() string { return "local" }

// Authenticate 校验用户名密码并返回启用状态的 User（含 Roles）。
// 用户不存在与密码错误统一返回 ErrInvalidCredential，防止枚举。
func (a *LocalAuthenticator) Authenticate(ctx context.Context, username, password string) (*store.User, error) {
	var u store.User
	err := a.db.WithContext(ctx).Preload("Roles").Where("username = ?", username).First(&u).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidCredential
		}
		return nil, err
	}
	if u.Status != store.UserStatusActive {
		return nil, ErrUserDisabled
	}
	if err := store.ComparePassword(u.PasswordHash, password); err != nil {
		return nil, ErrInvalidCredential
	}
	return &u, nil
}

// NewSessionStore 构造 cookie store（secret 来自 config.server.session_secret）。
func NewSessionStore(secret string) sessions.Store {
	return cookie.NewStore([]byte(secret))
}

// SessionMiddleware 返回 gin-contrib/sessions 的 Sessions 中间件，cookie 名为 "wrthub"。
func SessionMiddleware(secret string) gin.HandlerFunc {
	return sessions.Sessions(sessionName, NewSessionStore(secret))
}

// Login 将用户写入会话并设置 cookie 选项（HttpOnly、SameSite=Lax、12h）。
func Login(c *gin.Context, u *store.User) {
	s := sessions.Default(c)
	s.Clear()
	s.Set(sessionUIDKey, u.ID)
	s.Set(sessionUNameKey, u.Username)
	s.Options(sessions.Options{
		Path:     "/",
		MaxAge:   12 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	_ = s.Save()
}

// Logout 清空会话。
func Logout(c *gin.Context) {
	s := sessions.Default(c)
	s.Clear()
	_ = s.Save()
}

// CurrentUserID 从会话取用户 ID；未登录返回 0,false。
// 注意：session 存的是 uint（Login 设 u.ID 类型），类型断言用 uint。
func CurrentUserID(c *gin.Context) (uint, bool) {
	v := sessions.Default(c).Get(sessionUIDKey)
	if v == nil {
		return 0, false
	}
	id, ok := v.(uint)
	return id, ok
}

// CurrentUser 从 gin.Context 取 RequireAuth 注入的当前用户。
func CurrentUser(c *gin.Context) (*store.User, bool) {
	v, ok := c.Get(ctxUserKey)
	if !ok {
		return nil, false
	}
	u, ok := v.(*store.User)
	return u, ok
}

// RequireAuth 要求已登录；否则按请求类型拒绝：
//   - HTMX：401 + HX-Redirect: /login（前端拦截跳转）；
//   - JSON：401 JSON 错误；
//   - HTML：302 跳 /login。
//
// 同时从 DB 预加载当前用户（含 Roles）注入 context，便于后续 RBAC。
func RequireAuth(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := CurrentUserID(c)
		if !ok {
			deny(c)
			return
		}
		var u store.User
		if err := db.WithContext(c.Request.Context()).Preload("Roles").First(&u, uid).Error; err != nil {
			deny(c)
			return
		}
		if u.Status != store.UserStatusActive {
			Logout(c)
			deny(c)
			return
		}
		c.Set(ctxUserKey, &u)
		c.Next()
	}
}

func deny(c *gin.Context) {
	if isHTMX(c) {
		c.Header("HX-Redirect", "/login")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if wantsJSON(c) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.Redirect(http.StatusFound, "/login")
	c.Abort()
}

func isHTMX(c *gin.Context) bool { return c.GetHeader("HX-Request") == "true" }

func wantsJSON(c *gin.Context) bool {
	h := c.GetHeader("Accept")
	return strings.Contains(h, "application/json") || strings.Contains(h, "text/json")
}
