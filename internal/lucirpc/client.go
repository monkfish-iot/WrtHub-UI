package lucirpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrUnreachable 表示设备侧 Web/RPC 服务不可达：frp 隧道未连通（frps 对任意路径返回
// 404 HTML 页）、连接被拒绝、超时等。上层页面据此给出统一友好提示，而不是把
// frp/HTML 错误页原文拼进提示（设备离线时原实现会把整段 frp 404 页面塞进页面）。
var ErrUnreachable = errors.New("设备不可达")

// Options 是 LuCI RPC client 的配置（对应 config.device_rpc）。
type Options struct {
	Scheme         string
	DomainSuffix   string
	Port           int
	AuthPath       string
	RpcPathPrefix  string
	TokenCacheTTL  time.Duration
	Timeout        time.Duration
	MaxConcurrency int
	// HostResolver 可选：按 session_id 解析设备访问基地址（隧道/直连双模式）。
	// 为 nil 时固定使用 {session_id}.{DomainSuffix}:{Port}。
	HostResolver HostResolver
}

// HostResolver 按 session_id 返回设备访问基地址（不含路径），如
// http://{session_id}.frpclient.local:20001 或 http://192.168.1.1:8765。
type HostResolver interface {
	Host(ctx context.Context, sessionID string) (string, error)
}

// CredentialProvider 按 session_id 提供设备登录凭证。
// 实现方应仅内存短缓存，不得落盘。
type CredentialProvider interface {
	Credential(ctx context.Context, sessionId string) (username, password string, err error)
}

type tokenEntry struct {
	token   string
	expires time.Time
}

// Client 是 LuCI RPC 客户端，管理 token 缓存与单设备并发限流。
// 单请求超时以 atomic 存放、逐请求经 context 生效，支持 SetTimeout 运行时热更新。
type Client struct {
	http    *http.Client
	opts    Options
	creds   CredentialProvider
	log     *slog.Logger
	timeout atomic.Int64 // 单请求超时（纳秒）

	mu    sync.Mutex
	cache map[string]*tokenEntry

	semMu sync.Mutex
	sems  map[string]chan struct{}
}

// New 构造 LuCI RPC 客户端。log 用于打印鉴权/RPC 失败的诊断日志，
// 便于排查"luci auth failed: <nil>"类问题（实际响应体片段会随日志输出，
// 但不会打印 password / token 明文）。
func New(opts Options, creds CredentialProvider, log *slog.Logger) *Client {
	applyDefaults(&opts)
	if log == nil {
		log = slog.Default()
	}
	c := &Client{
		http:  &http.Client{},
		opts:  opts,
		creds: creds,
		log:   log,
		cache: make(map[string]*tokenEntry),
		sems:  make(map[string]chan struct{}),
	}
	c.timeout.Store(int64(opts.Timeout))
	return c
}

// SetTimeout 热更新单请求超时（作用于后续请求；<=0 忽略）。
func (c *Client) SetTimeout(d time.Duration) {
	if d > 0 {
		c.timeout.Store(int64(d))
	}
}

// withTimeout 用当前超时包装 ctx（逐请求生效，配合 SetTimeout 热更新）。
// 调用方须在响应体读取完成后再 cancel（典型：函数顶 defer cancel()）。
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(c.timeout.Load()))
}

// SetTokenCacheTTL 热更新 token 缓存 TTL（作用于新写入的缓存条目；<=0 忽略）。
func (c *Client) SetTokenCacheTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	c.opts.TokenCacheTTL = d
	c.mu.Unlock()
}

func applyDefaults(o *Options) {
	if o.Scheme == "" {
		o.Scheme = "http"
	}
	if o.DomainSuffix == "" {
		o.DomainSuffix = "frpclient.local"
	}
	if o.Port == 0 {
		o.Port = 20001
	}
	if o.AuthPath == "" {
		o.AuthPath = "/auth"
	}
	if o.RpcPathPrefix == "" {
		o.RpcPathPrefix = "/cgi-bin/luci/rpc"
	}
	if o.TokenCacheTTL == 0 {
		o.TokenCacheTTL = 5 * time.Minute
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	if o.MaxConcurrency == 0 {
		o.MaxConcurrency = 4
	}
}

// Host 返回指定设备的访问基地址：配置了 HostResolver 时按其解析（隧道/直连双模式），
// 否则固定 {session_id}.{domain_suffix}:{port}。
func (c *Client) Host(ctx context.Context, sessionID string) (string, error) {
	if c.opts.HostResolver != nil {
		return c.opts.HostResolver.Host(ctx, sessionID)
	}
	return c.defaultHost(sessionID), nil
}

func (c *Client) defaultHost(sessionID string) string {
	return fmt.Sprintf("%s://%s.%s:%d", c.opts.Scheme, sessionID, c.opts.DomainSuffix, c.opts.Port)
}

// Auth 调用设备 /auth 换取 RPC token。
// 失败时打日志含 session_id / HTTP 状态 / 响应体前 256 字节，便于排查
// （设备可能返回 HTML 错误页 / nginx 拦截页 / 非 JSON 结构，原代码只报 "<nil>" 无法定位）。
func (c *Client) Auth(ctx context.Context, sessionID, username, password string) (string, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	base, err := c.Host(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("resolve device host: %w", err)
	}
	body, _ := json.Marshal(authRequest{Username: username, Password: password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+c.opts.AuthPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("luci auth: http request failed",
			"session_id", sessionID, "url", base+c.opts.AuthPath, "err", err)
		return "", unreachableErr("luci auth", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.log.Warn("luci auth: non-200",
			"session_id", sessionID, "status", resp.StatusCode, "body_head", head(data, 256))
		return "", non200Err("luci auth", resp.StatusCode, data)
	}
	var ar authResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		c.log.Warn("luci auth: decode failed",
			"session_id", sessionID, "err", err, "body_head", head(data, 256))
		return "", fmt.Errorf("decode auth response: %w (body head: %s)", err, head(data, 256))
	}
	// 实际响应：{"approved":true,"auth_token":"...","luci_token":"<RPC_TOKEN>","username":""}
	// 用 approved 判断登录是否通过，luci_token 作为 RPC 凭据。
	if !ar.Approved {
		c.log.Warn("luci auth: not approved",
			"session_id", sessionID, "body_head", head(data, 256))
		return "", fmt.Errorf("luci auth not approved (username/password rejected)")
	}
	if ar.LuciToken == "" {
		c.log.Warn("luci auth: empty luci_token",
			"session_id", sessionID, "body_head", head(data, 256))
		return "", fmt.Errorf("luci auth returned empty luci_token")
	}
	c.log.Debug("luci auth: ok", "session_id", sessionID, "token_len", len(ar.LuciToken))
	return ar.LuciToken, nil
}

// getToken 取有效 token，缓存命中则不触发 /auth。
func (c *Client) getToken(ctx context.Context, sessionID string) (string, error) {
	c.mu.Lock()
	if e, ok := c.cache[sessionID]; ok && time.Now().Before(e.expires) {
		tok := e.token
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	if c.creds == nil {
		return "", fmt.Errorf("no credential provider")
	}
	username, password, err := c.creds.Credential(ctx, sessionID)
	if err != nil {
		c.log.Warn("get credential failed", "session_id", sessionID, "err", err)
		return "", fmt.Errorf("get credential: %w", err)
	}
	tok, err := c.Auth(ctx, sessionID, username, password)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.cache[sessionID] = &tokenEntry{token: tok, expires: time.Now().Add(c.opts.TokenCacheTTL)}
	c.mu.Unlock()
	return tok, nil
}

func (c *Client) invalidate(sessionID string) {
	c.mu.Lock()
	delete(c.cache, sessionID)
	c.mu.Unlock()
}

// Token 返回指定设备的有效 token（缓存命中则不重新 /auth）。
// 供 ubusrpc 复用同一凭据：实测 /auth 返回的 luci_token 可直接作为 /ubus
// JSON-RPC 调用的 params[0] token，无需再走 ubus session.login。
func (c *Client) Token(ctx context.Context, sessionID string) (string, error) {
	return c.getToken(ctx, sessionID)
}

// Invalidate 清除指定设备的 token 缓存（下次调用重新 /auth）。
// 供 ubusrpc 在 /ubus 侧会话失效（result[0]=6 / 401/403）时联动刷新。
func (c *Client) Invalidate(sessionID string) {
	c.invalidate(sessionID)
}

// callRPC 单次 RPC 调用；retry=true 表示疑似鉴权失效，应清缓存重试。
func (c *Client) callRPC(ctx context.Context, sessionID, namespace, token string, reqID int, method string, params []any) (json.RawMessage, bool, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	base, err := c.Host(ctx, sessionID)
	if err != nil {
		return nil, false, fmt.Errorf("resolve device host: %w", err)
	}
	body, _ := json.Marshal(rpcRequest{ID: reqID, Method: method, Params: params})
	u := fmt.Sprintf("%s%s/%s?auth=%s", base, c.opts.RpcPathPrefix, namespace, url.QueryEscape(token))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.log.Warn("luci rpc: http request failed",
			"session_id", sessionID, "ns", namespace, "method", method, "err", err)
		return nil, false, unreachableErr("luci rpc", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.log.Warn("luci rpc: auth-status",
			"session_id", sessionID, "ns", namespace, "method", method, "status", resp.StatusCode)
		return nil, true, fmt.Errorf("luci rpc http %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Warn("luci rpc: non-200",
			"session_id", sessionID, "ns", namespace, "method", method,
			"status", resp.StatusCode, "body_head", head(data, 256))
		return nil, false, non200Err("luci rpc", resp.StatusCode, data)
	}
	var rr rpcResponse
	if err := json.Unmarshal(data, &rr); err != nil {
		c.log.Warn("luci rpc: decode failed",
			"session_id", sessionID, "ns", namespace, "method", method,
			"err", err, "body_head", head(data, 256))
		return nil, false, fmt.Errorf("decode rpc response: %w (body head: %s)", err, head(data, 256))
	}
	if rr.Error != nil {
		errStr := fmt.Sprintf("%v", rr.Error)
		c.log.Warn("luci rpc: device error",
			"session_id", sessionID, "ns", namespace, "method", method, "error", errStr)
		return nil, isAuthError(errStr), fmt.Errorf("luci rpc error: %s", errStr)
	}
	c.log.Debug("luci rpc: ok",
		"session_id", sessionID, "ns", namespace, "method", method, "result_len", len(rr.Result))
	return rr.Result, false, nil
}

// Call 调用 LuCI RPC：自动管理 token 与单设备并发限流，遇鉴权失效自动重试一次。
func (c *Client) Call(ctx context.Context, sessionID, namespace, method string, params ...any) (json.RawMessage, error) {
	release := c.acquire(sessionID)
	defer release()

	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.getToken(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		result, retry, err := c.callRPC(ctx, sessionID, namespace, tok, 1, method, params)
		if err == nil {
			return result, nil
		}
		if !retry {
			return nil, err
		}
		c.log.Info("luci rpc: retrying after token invalidate",
			"session_id", sessionID, "ns", namespace, "method", method, "attempt", attempt+1)
		c.invalidate(sessionID)
	}
	return nil, fmt.Errorf("luci rpc failed after retry: %s/%s", namespace, method)
}

// CallInto 调用 RPC 并把 result 解析到 out 指针。
func (c *Client) CallInto(ctx context.Context, sessionID, namespace, method string, out any, params ...any) error {
	raw, err := c.Call(ctx, sessionID, namespace, method, params...)
	if err != nil {
		return err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// acquire 获取单设备并发槽，返回释放函数。
func (c *Client) acquire(sessionID string) func() {
	c.semMu.Lock()
	sem, ok := c.sems[sessionID]
	if !ok {
		sem = make(chan struct{}, c.opts.MaxConcurrency)
		c.sems[sessionID] = sem
	}
	c.semMu.Unlock()
	sem <- struct{}{}
	return func() { <-sem }
}

// isAuthError 返回 true 表示错误文本疑似鉴权失效。
func isAuthError(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "session") || strings.Contains(l, "denied") ||
		strings.Contains(l, "unauthorized") || strings.Contains(l, "auth")
}

// unreachableErr 包装请求失败（连接被拒绝/超时/DNS 失败等）：对使用者一律视为设备不可达。
func unreachableErr(action string, err error) error {
	return fmt.Errorf("%w：%s 请求失败（%v）", ErrUnreachable, action, err)
}

// non200Err 构造非 200 响应错误：只回显简短原因（完整响应体见日志），
// 隧道不可达类响应（frps 404 页/HTML 错误页）包装 ErrUnreachable 供上层统一提示。
func non200Err(action string, status int, body []byte) error {
	hint, unreachable := replyHint(body)
	err := fmt.Errorf("%s http %d（%s）", action, status, hint)
	if unreachable {
		return fmt.Errorf("%w：%v", ErrUnreachable, err)
	}
	return err
}

// replyHint 从未预期的响应体推断简短原因；unreachable 表示隧道不可达或设备 Web 服务未运行。
func replyHint(body []byte) (hint string, unreachable bool) {
	switch {
	case bytes.Contains(body, []byte("powered by frp")):
		return "设备隧道未连通，设备可能正在重启或离线", true
	case bytes.Contains(body, []byte("<html")), bytes.Contains(body, []byte("<!DOCTYPE")):
		return "设备返回了非预期页面，可能未运行 Web/RPC 服务", true
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return head([]byte(s), 120), false
	}
	return "空响应", false
}

// head 返回字节切片的前 n 字节字符串（用于日志诊断，避免响应体过长淹没日志）。
func head(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
