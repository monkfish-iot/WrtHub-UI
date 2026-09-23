package lucirpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

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
type Client struct {
	http  *http.Client
	opts  Options
	creds CredentialProvider
	log   *slog.Logger

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
	return &Client{
		http:  &http.Client{Timeout: opts.Timeout},
		opts:  opts,
		creds: creds,
		log:   log,
		cache: make(map[string]*tokenEntry),
		sems:  make(map[string]chan struct{}),
	}
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

func (c *Client) host(sessionID string) string {
	return fmt.Sprintf("%s://%s.%s:%d", c.opts.Scheme, sessionID, c.opts.DomainSuffix, c.opts.Port)
}

// Auth 调用设备 /auth 换取 RPC token。
// 失败时打日志含 session_id / HTTP 状态 / 响应体前 256 字节，便于排查
// （设备可能返回 HTML 错误页 / nginx 拦截页 / 非 JSON 结构，原代码只报 "<nil>" 无法定位）。
func (c *Client) Auth(ctx context.Context, sessionID, username, password string) (string, error) {
	body, _ := json.Marshal(authRequest{Username: username, Password: password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host(sessionID)+c.opts.AuthPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("luci auth: http request failed",
			"session_id", sessionID, "url", c.host(sessionID)+c.opts.AuthPath, "err", err)
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.log.Warn("luci auth: non-200",
			"session_id", sessionID, "status", resp.StatusCode, "body_head", head(data, 256))
		return "", fmt.Errorf("luci auth http %d: %s", resp.StatusCode, string(data))
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
	body, _ := json.Marshal(rpcRequest{ID: reqID, Method: method, Params: params})
	u := fmt.Sprintf("%s%s/%s?auth=%s", c.host(sessionID), c.opts.RpcPathPrefix, namespace, url.QueryEscape(token))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.log.Warn("luci rpc: http request failed",
			"session_id", sessionID, "ns", namespace, "method", method, "err", err)
		return nil, false, err
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
		return nil, false, fmt.Errorf("luci rpc http %d: %s", resp.StatusCode, string(data))
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

func isAuthError(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "session") || strings.Contains(l, "denied") ||
		strings.Contains(l, "unauthorized") || strings.Contains(l, "auth")
}

// head 返回字节切片的前 n 字节字符串（用于日志诊断，避免响应体过长淹没日志）。
func head(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
