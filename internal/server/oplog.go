package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/store"
)

const (
	oplogBodyLimit   = 32 * 1024 // 请求体快照上限（超出存截断标记）
	oplogRespLimit   = 2 * 1024  // 响应体捕获上限（提取失败信息用）
	oplogErrMsgLimit = 512       // 错误信息落库上限
)

// oplogMiddleware 拦截所有写操作（POST/PUT/DELETE/PATCH），统一记录操作日志。
//
// 设计要点（需求：写/改操作统一拦截，保证不漏）：
//   - 路由层统一挂载（SessionMiddleware 之后），新增写接口自动纳入，无需逐个 handler 感知；
//   - 身份在 c.Next() 之前抓取：/logout 的 handler 会清空会话；/login 尚无会话则回退表单 username；
//   - 请求体读取后回填 c.Request.Body，handler 仍可正常解析；敏感字段打码后入库存快照；
//   - 结果判定：优先解析 302 重定向 Location 的 ok=/err= 回显（本项目写操作约定），否则看 HTTP 状态码；
//   - 落库失败仅告警，不影响主流程（append-only，无修改/删除入口）。
func oplogMiddleware(app *App) gin.HandlerFunc {
	return func(c *gin.Context) {
		method := c.Request.Method
		if !isWriteMethod(method) {
			c.Next()
			return
		}
		start := time.Now()

		uid, _ := auth.CurrentUserID(c)
		uname, _ := auth.CurrentUsername(c)

		bodySnap, form := captureBody(c)
		if uname == "" && form != nil {
			if s, ok := form["username"].(string); ok {
				uname = s
			}
		}

		// 包装 ResponseWriter，捕获状态码与响应片段
		rw := &oplogRespWriter{ResponseWriter: c.Writer}
		c.Writer = rw

		c.Next()

		rec := buildOpLog(c, uid, uname, method, bodySnap, rw, time.Since(start))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.RecordOpLog(app.DB.WithContext(ctx), rec); err != nil {
			app.Log.Warn("operation log insert failed", "path", rec.Path, "err", err)
		}
	}
}

func isWriteMethod(m string) bool {
	return m == http.MethodPost || m == http.MethodPut || m == http.MethodDelete || m == http.MethodPatch
}

// buildOpLog 组装一条操作日志（身份/分类/结果）。
func buildOpLog(c *gin.Context, uid uint, uname, method string, bodySnap []byte, rw *oplogRespWriter, elapsed time.Duration) *store.AuditLog {
	status := rw.Status()
	path := c.Request.URL.Path
	module, action, targetType := classifyOp(method, path)
	targetID := c.Param("id")
	if targetID == "" {
		targetID = c.Param("device_id")
	}

	rec := &store.AuditLog{
		UserID:      uid,
		Username:    uname,
		Module:      module,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		Method:      method,
		Path:        path,
		Query:       c.Request.URL.RawQuery,
		RequestBody: bodySnap,
		Result:      "success",
		HTTPStatus:  status,
		ClientIP:    c.ClientIP(),
		ElapsedMS:   int(elapsed.Milliseconds()),
		CreatedAt:   time.Now(),
	}

	// 结果判定：优先 302 重定向的 ok=/err= 回显（本项目写操作统一约定）
	if loc := c.Writer.Header().Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil {
			switch {
			case u.Query().Get("err") != "":
				rec.Result = "fail"
				rec.ErrorMsg = truncateStr(u.Query().Get("err"), oplogErrMsgLimit)
			case u.Path == "/login" && path != "/login" && path != "/logout":
				// 受保护路由被 RequireAuth 拒绝（未登录/会话失效）→ 重定向 /login
				rec.Result = "fail"
				rec.ErrorMsg = "未登录或会话失效"
			}
		}
	}
	if status >= http.StatusBadRequest {
		rec.Result = "fail"
		if rec.ErrorMsg == "" {
			rec.ErrorMsg = truncateStr(strings.TrimSpace(string(rw.body.Bytes())), oplogErrMsgLimit)
		}
	}
	// 登录失败回显仅 err=1，补一条可读信息
	if rec.Result == "fail" && path == "/login" {
		rec.ErrorMsg = "用户名或密码错误"
	}
	return rec
}

// classifyOp 从请求路径推导 module/action/target_type。
// 已知路由给语义化动作名；未命中走通用兜底（module=首段, action=method 小写），保证新增接口不漏。
func classifyOp(method, path string) (module, action, targetType string) {
	switch path {
	case "/login":
		return "auth", "login", "user"
	case "/logout":
		return "auth", "logout", "user"
	}

	seg := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(seg) == 0 || seg[0] == "" {
		return "other", strings.ToLower(method), "other"
	}
	module = seg[0]
	targetType = module

	if module == "devices" {
		targetType = "device"
		if method == http.MethodPost {
			switch {
			case len(seg) == 4 && seg[2] == "config" && seg[3] == "reboot":
				return module, "reboot", targetType
			case len(seg) == 5 && seg[2] == "config" && seg[3] == "clients" && seg[4] == "kick":
				return module, "kick_client", targetType
			case len(seg) == 5 && seg[2] == "config" && seg[3] == "wireless" && seg[4] == "radio":
				return module, "radio_edit", targetType
			case len(seg) == 5 && seg[2] == "config" && seg[3] == "wireless" && seg[4] == "ssid":
				return module, "ssid_save", targetType
			case len(seg) == 6 && seg[2] == "config" && seg[3] == "wireless" && seg[4] == "ssid" && seg[5] == "delete":
				return module, "ssid_delete", targetType
			case len(seg) == 5 && seg[2] == "config" && seg[3] == "firmware" && seg[4] == "upgrade":
				return module, "firmware_upgrade", targetType
			}
		}
		if method == http.MethodDelete && len(seg) == 5 && seg[2] == "config" && seg[3] == "wireless" && seg[4] == "ssid" {
			return module, "ssid_delete", targetType
		}
	}

	// 通用动作推导
	switch {
	case method == http.MethodPost:
		action = "create"
	case method == http.MethodPut, method == http.MethodPatch:
		action = "update"
	case method == http.MethodDelete:
		action = "delete"
	default:
		action = strings.ToLower(method)
	}
	return module, action, targetType
}

// captureBody 读取并回填请求体，返回脱敏后的 JSON 快照与顶层键值（用于提取 username）。
// 支持 JSON 与 form-urlencoded；multipart/超限/非结构化存说明标记；敏感字段打码。
func captureBody(c *gin.Context) ([]byte, map[string]any) {
	if c.Request.Body == nil {
		return []byte("{}"), nil
	}
	if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
		return []byte(`{"_note":"multipart 请求体不记录"}`), nil
	}

	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, oplogBodyLimit+1))
	c.Request.Body = io.NopCloser(bytes.NewReader(raw)) // 回填，handler 正常读取
	if err != nil {
		return []byte(`{"_note":"请求体读取失败"}`), nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []byte("{}"), nil
	}
	if len(raw) > oplogBodyLimit {
		return []byte(`{"_note":"请求体超过记录上限，已省略"}`), nil
	}

	// JSON 请求体
	if strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") || json.Valid(trimmed) {
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return []byte(`{"_note":"非 JSON 请求体不记录"}`), nil
		}
		masked := maskValue(v)
		b, err := json.Marshal(masked)
		if err != nil {
			return []byte("{}"), nil
		}
		if m, ok := masked.(map[string]any); ok {
			return b, m
		}
		return b, nil
	}

	// form-urlencoded
	vals, err := url.ParseQuery(string(trimmed))
	if err != nil {
		return []byte(`{"_note":"非结构化请求体不记录"}`), nil
	}
	form := make(map[string]any, len(vals))
	for k, v := range vals {
		if isSensitiveKey(k) {
			form[k] = "***"
			continue
		}
		form[k] = strings.Join(v, ",")
	}
	b, err := json.Marshal(form)
	if err != nil {
		return []byte("{}"), nil
	}
	return b, form
}

// maskValue 递归打码敏感字段（用于 JSON 请求体）。
func maskValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSensitiveKey(k) {
				out[k] = "***"
				continue
			}
			out[k] = maskValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskValue(val)
		}
		return out
	default:
		return v
	}
}

// isSensitiveKey 判断字段名是否为敏感字段（key 含 password/passwd/secret/token/private_key/psk/key）。
func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range []string{"password", "passwd", "secret", "token", "private_key", "passphrase", "psk", "key"} {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}

// truncateStr 按字节截断（用于 error_msg 落库上限）。
func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// oplogRespWriter 包装 gin.ResponseWriter，捕获响应体前 oplogRespLimit 字节。
type oplogRespWriter struct {
	gin.ResponseWriter
	body bytes.Buffer
}

func (w *oplogRespWriter) Write(b []byte) (int, error) {
	if w.body.Len() < oplogRespLimit {
		remain := oplogRespLimit - w.body.Len()
		if len(b) <= remain {
			w.body.Write(b)
		} else {
			w.body.Write(b[:remain])
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *oplogRespWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}
