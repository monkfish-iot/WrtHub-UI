package server

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/store"
)

// userRow 是用户列表行：附角色名/角色 ID、最近登录时间与成功登录次数（取自审计日志）。
type userRow struct {
	store.User
	RoleNames  string
	RoleIDs    []uint
	LastLogin  string
	LoginCount int64
}

// usersPage 是「本地账号（平台用户）」页面视图。
type usersPage struct {
	pageBase
	Users     []userRow
	Roles     []store.Role
	Keyword   string
	FlashOK   string
	FlashErr  string
	LogsUser  *userRow // 非空表示展示该账号的登录日志（?logs=<id>）
	Logs      []store.AuditLog
	LogsTotal int64
}

// loginStat 是登录统计（按用户名聚合审计日志）。
type loginStat struct {
	Username string
	Cnt      int64
	Last     time.Time
}

// handleUsersList 渲染本地账号列表；?logs=<id> 时同时展示该账号的登录日志。
func (a *App) handleUsersList(c *gin.Context) {
	u, _ := auth.CurrentUser(c)
	pg := usersPage{
		pageBase: pageBase{ActiveMenu: "users", CurrentUser: u, Config: a.Cfg},
		Keyword:  strings.TrimSpace(c.Query("keyword")),
		FlashOK:  c.Query("ok"),
		FlashErr: c.Query("err"),
	}

	var us []store.User
	q := a.DB.Preload("Roles").Order("id asc")
	if pg.Keyword != "" {
		like := "%" + pg.Keyword + "%"
		q = q.Where("username LIKE ? OR email LIKE ?", like, like)
	}
	if err := q.Find(&us).Error; err != nil {
		a.Log.Error("list users failed", "err", err)
		pg.FlashErr = "加载用户失败：" + err.Error()
	}

	// 登录统计（一次聚合查询，避免逐用户查询）
	stats := make(map[string]loginStat, len(us))
	var sts []loginStat
	if err := a.DB.Model(&store.AuditLog{}).
		Select("username, count(*) AS cnt, max(created_at) AS last").
		Where("module = ? AND action = ? AND result = ?", "auth", "login", "success").
		Group("username").Scan(&sts).Error; err != nil {
		a.Log.Warn("aggregate login stats failed", "err", err)
	}
	for _, s := range sts {
		stats[s.Username] = s
	}

	for i := range us {
		row := userRow{User: us[i], LastLogin: "-"}
		names := make([]string, 0, len(us[i].Roles))
		for _, r := range us[i].Roles {
			names = append(names, r.Name)
			row.RoleIDs = append(row.RoleIDs, r.ID)
		}
		row.RoleNames = strings.Join(names, "、")
		if s, ok := stats[us[i].Username]; ok {
			row.LoginCount = s.Cnt
			if !s.Last.IsZero() {
				row.LastLogin = s.Last.Format("2006-01-02 15:04:05")
			}
		}
		pg.Users = append(pg.Users, row)
	}

	if err := a.DB.Order("code asc").Find(&pg.Roles).Error; err != nil {
		a.Log.Warn("list roles failed", "err", err)
	}

	if idStr := c.Query("logs"); idStr != "" {
		id := atoiDefault(idStr, 0)
		for i := range pg.Users {
			if pg.Users[i].ID == uint(id) {
				lu := pg.Users[i]
				pg.LogsUser = &lu
				break
			}
		}
		if pg.LogsUser != nil {
			// 登录/登出记录：module=auth，成功与失败都展示（登录 IP 即 client_ip）
			a.DB.Where("username = ? AND module = ? AND action IN ?",
				pg.LogsUser.Username, "auth", []string{"login", "logout"}).
				Order("created_at desc").Limit(200).Find(&pg.Logs)
			a.DB.Model(&store.AuditLog{}).
				Where("username = ? AND module = ? AND action IN ?",
					pg.LogsUser.Username, "auth", []string{"login", "logout"}).
				Count(&pg.LogsTotal)
		}
	}

	c.Status(http.StatusOK)
	if rerr := a.Render.Render(c.Writer, "pages/users", pg); rerr != nil {
		a.Log.Error("render users page failed", "err", rerr)
	}
}

// handleUserCreate 新建本地账号（POST /users）。
func (a *App) handleUserCreate(c *gin.Context) {
	back := "/users"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+urlQuery(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+urlQuery(msg)) }

	username := strings.TrimSpace(c.PostForm("username"))
	password := c.PostForm("password")
	if username == "" || len(username) > 64 {
		errMsg("用户名不能为空且不超过 64 个字符")
		return
	}
	if len(password) < 6 {
		errMsg("密码至少 6 位")
		return
	}
	var cnt int64
	a.DB.Model(&store.User{}).Where("username = ?", username).Count(&cnt)
	if cnt > 0 {
		errMsg("用户名已存在：" + username)
		return
	}

	hash, err := store.HashPassword(password)
	if err != nil {
		a.Log.Error("hash password failed", "err", err)
		errMsg("创建失败：密码哈希失败")
		return
	}
	user := store.User{
		Username:     username,
		PasswordHash: hash,
		Email:        strings.TrimSpace(c.PostForm("email")),
		Status:       parseStatus(c.PostForm("status")),
	}
	if err := a.DB.Create(&user).Error; err != nil {
		a.Log.Error("create user failed", "err", err)
		errMsg("创建失败：" + err.Error())
		return
	}
	if err := a.setUserRoles(user.ID, c.PostFormArray("roles")); err != nil {
		a.Log.Error("set user roles failed", "user", username, "err", err)
		errMsg("账号已创建，但角色绑定失败：" + err.Error())
		return
	}
	a.Log.Info("user created", "username", username, "operator", currentName(c))
	okMsg("账号 " + username + " 已创建")
}

// handleUserUpdate 编辑本地账号（POST /users/:id）：
// 邮箱/状态/角色可改，密码留空表示保留原值；不允许禁用或取消自己的管理员角色。
func (a *App) handleUserUpdate(c *gin.Context) {
	id := atoiDefault(c.Param("id"), 0)
	back := "/users"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+urlQuery(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+urlQuery(msg)) }

	var user store.User
	if err := a.DB.First(&user, id).Error; err != nil {
		errMsg("账号不存在")
		return
	}
	me, _ := auth.CurrentUser(c)
	isSelf := me != nil && me.ID == user.ID

	status := parseStatus(c.PostForm("status"))
	if isSelf && status == 0 {
		errMsg("不能禁用当前登录账号")
		return
	}

	updates := map[string]any{
		"email":  strings.TrimSpace(c.PostForm("email")),
		"status": status,
	}
	if pwd := c.PostForm("password"); pwd != "" {
		if len(pwd) < 6 {
			errMsg("密码至少 6 位")
			return
		}
		hash, herr := store.HashPassword(pwd)
		if herr != nil {
			a.Log.Error("hash password failed", "err", herr)
			errMsg("保存失败：密码哈希失败")
			return
		}
		updates["password_hash"] = hash
	}
	if err := a.DB.Model(&user).Updates(updates).Error; err != nil {
		a.Log.Error("update user failed", "id", user.ID, "err", err)
		errMsg("保存失败：" + err.Error())
		return
	}
	// 编辑自己时不改角色，避免误操作把自己踢出管理员
	if !isSelf {
		if err := a.setUserRoles(user.ID, c.PostFormArray("roles")); err != nil {
			a.Log.Error("set user roles failed", "user", user.Username, "err", err)
			errMsg("资料已保存，但角色绑定失败：" + err.Error())
			return
		}
	}
	a.Log.Info("user updated", "id", user.ID, "username", user.Username, "operator", currentName(c))
	okMsg("账号 " + user.Username + " 已保存")
}

// handleUserToggle 启用/禁用本地账号（POST /users/:id/toggle）。
func (a *App) handleUserToggle(c *gin.Context) {
	id := atoiDefault(c.Param("id"), 0)
	back := "/users"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+urlQuery(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+urlQuery(msg)) }

	var user store.User
	if err := a.DB.First(&user, id).Error; err != nil {
		errMsg("账号不存在")
		return
	}
	me, _ := auth.CurrentUser(c)
	if me != nil && me.ID == user.ID && user.Status == 1 {
		errMsg("不能禁用当前登录账号")
		return
	}
	newStatus := 0
	if user.Status == 0 {
		newStatus = 1
	}
	if err := a.DB.Model(&user).Update("status", newStatus).Error; err != nil {
		a.Log.Error("toggle user failed", "id", user.ID, "err", err)
		errMsg("操作失败：" + err.Error())
		return
	}
	state := "已启用"
	if newStatus == 0 {
		state = "已禁用"
	}
	a.Log.Info("user toggled", "id", user.ID, "username", user.Username, "status", newStatus, "operator", currentName(c))
	okMsg("账号 " + user.Username + " " + state)
}

// handleUserDelete 删除本地账号（POST /users/:id/delete）；不允许删除当前登录账号。
func (a *App) handleUserDelete(c *gin.Context) {
	id := atoiDefault(c.Param("id"), 0)
	back := "/users"
	okMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?ok="+urlQuery(msg)) }
	errMsg := func(msg string) { c.Redirect(http.StatusFound, back+"?err="+urlQuery(msg)) }

	var user store.User
	if err := a.DB.First(&user, id).Error; err != nil {
		errMsg("账号不存在")
		return
	}
	me, _ := auth.CurrentUser(c)
	if me != nil && me.ID == user.ID {
		errMsg("不能删除当前登录账号")
		return
	}
	if err := a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", user.ID).Delete(&store.UserRole{}).Error; err != nil {
			return err
		}
		return tx.Delete(&user).Error
	}); err != nil {
		a.Log.Error("delete user failed", "id", user.ID, "err", err)
		errMsg("删除失败：" + err.Error())
		return
	}
	a.Log.Info("user deleted", "id", user.ID, "username", user.Username, "operator", currentName(c))
	okMsg("账号 " + user.Username + " 已删除")
}

// setUserRoles 以给定角色 ID 全量替换用户的角色绑定。
func (a *App) setUserRoles(userID uint, roleIDs []string) error {
	return a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", userID).Delete(&store.UserRole{}).Error; err != nil {
			return err
		}
		for _, rs := range roleIDs {
			rid := atoiDefault(rs, 0)
			if rid <= 0 {
				continue
			}
			var cnt int64
			if err := tx.Model(&store.Role{}).Where("id = ?", rid).Count(&cnt).Error; err != nil {
				return err
			}
			if cnt == 0 {
				continue
			}
			if err := tx.Create(&store.UserRole{UserID: userID, RoleID: uint(rid)}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// parseStatus 表单状态值转状态码（1 启用 / 0 禁用）。
func parseStatus(s string) int {
	if s == "1" {
		return 1
	}
	return 0
}

// currentName 返回当前操作人名（用于业务日志）。
func currentName(c *gin.Context) string {
	if n, ok := auth.CurrentUsername(c); ok {
		return n
	}
	return ""
}

// urlQuery 表单回显参数转义（中文/特殊字符不破坏 Location）。
func urlQuery(s string) string { return url.QueryEscape(s) }
