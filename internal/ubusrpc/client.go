// Package ubusrpc 实现 UBUS JSON-RPC 客户端，作为 lucirpc 失败时的兜底访问方法。
//
// 实测确认（frps_helper 场景，设备 OpenWrt 25.12.2 / NatShell WR3000k）：
// /auth 返回的 luci_token 可直接作为 /ubus JSON-RPC 调用的 params[0] token，
// 无需再走 ubus session.login 换独立 ubus_rpc_session：
//
//	curl http://{sid}:20001/ubus -d '{"jsonrpc":"2.0","id":1,"method":"call",
//	  "params":["<luci_token>","system","board",{}]}'
//	→ {"jsonrpc":"2.0","id":1,"result":[0,{"hostname":"OpenWrt",...}]}
//
// 因此本客户端不独立管理 token，直接复用 lucirpc.Client 的 token 缓存；
// /ubus 侧会话失效（result[0]=6 或 HTTP 401/403）时联动 lucirpc 失效并重取一次。
//
// 协议要点：
//
//	URL:  POST http://{sid}.frpclient.local:20001/ubus
//	业务: {"jsonrpc":"2.0","id":1,"method":"call","params":[token,"iwinfo","assoclist",{"device":"phy0-ap0"}]}
//	      响应 result=[0, {数据}] 或 result=[errno, null]
//	      ubus 错误码：4=NOT_FOUND（如接口名不存在）、6=PERMISSION_DENIED（token 失效/ACL 拒绝）
//	      协议层错误：{"error":{"code":-32002,"message":"Access denied"}}（如对象未安装/ACL 不允许）
//
//	空数据兜底（用户要求）：result[0]=0 但 result[1] 无数据（null/{}/已知列表键全空数组，
//	如 {"results":[]}）时，视为疑似 token 失效，联动 lucirpc 清缓存重新 /auth 再试一次；
//	仍为空则按真实空数据返回（设备该接口确实无接入终端/扫描结果），不报错。
//
// iwinfo ubus 对象方法由 C 实现（不依赖 luci lua 模块），在缺 luci.model.network 的固件上仍可用；
// 需设备安装 rpcd-mod-iwinfo 扩展，否则返回 -32002 Access denied。
package ubusrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"wrthub-ui/internal/lucirpc"
)

// Options 是 UBUS client 的配置。
// Scheme/DomainSuffix/Port/Timeout/MaxConcurrency 与 lucirpc 共用同一组值，
// 仅 UbusPath 单独配置（默认 /ubus）。token 复用 lucirpc，无独立 TTL。
type Options struct {
	Scheme         string
	DomainSuffix   string
	Port           int
	UbusPath       string
	Timeout        time.Duration
	MaxConcurrency int
}

// Client 是 UBUS JSON-RPC 客户端，作为 lucirpc 失败时的兜底访问方法。
// token 来源为 lucirpc.Client（/auth 的 luci_token），不独立缓存。
type Client struct {
	http *http.Client
	opts Options
	rpc  *lucirpc.Client // token 来源（/auth luci_token，实测可直接用于 /ubus）
	log  *slog.Logger

	semMu sync.Mutex
	sems  map[string]chan struct{}
}

// New 构造 UBUS 客户端。rpc 提供并管理 token（缓存/失效/重取），
// log 用于打印调用失败诊断日志（含响应体前 256 字节，不含 password/token 明文）。
func New(opts Options, rpc *lucirpc.Client, log *slog.Logger) *Client {
	applyDefaults(&opts)
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		http: &http.Client{Timeout: opts.Timeout},
		opts: opts,
		rpc:  rpc,
		log:  log,
		sems: make(map[string]chan struct{}),
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
	if o.UbusPath == "" {
		o.UbusPath = "/ubus"
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

// callUbus 单次 ubus 调用；retry=true 表示疑似 token 失效（result[0]=6 或 HTTP 401/403），应联动失效重试；
// empty=true 表示调用成功但无数据（result[0]=0 且 result[1] 为空），由 Call 决定是否重取 token 重试。
// 所有日志均带完整请求 URL 与响应体片段，便于对照手工 curl 排查（用户要求）。
func (c *Client) callUbus(ctx context.Context, sessionID, token string, reqID int, obj, method string, params map[string]any) (json.RawMessage, bool, bool, error) {
	u := c.host(sessionID) + c.opts.UbusPath
	body, _ := json.Marshal(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      reqID,
		Method:  "call",
		Params:  []any{token, obj, method, params},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, false, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	// 用户要求：打印完整请求 URL + POST body（body 含 params[0] 的 token，
	// 便于与手工 curl 逐字对照排查；token 属设备侧凭据，用户自持）。
	// 另附可直接粘贴执行的 curl 命令（json.Marshal 输出不含单引号，单引号包裹安全），
	// 避免从日志复制 body 时带上 Go 转义符 \" 导致 -32700 Parse error。
	c.log.Info("ubus: call", "url", u, "body", string(body),
		"curl", fmt.Sprintf("curl -X POST '%s' -H 'Content-Type: application/json' -d '%s'", u, string(body)))
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("ubus: http request failed",
			"url", u, "session_id", sessionID, "obj", obj, "method", method, "err", err)
		return nil, false, false, err
	}
	defer resp.Body.Close()
	dataBytes, _ := io.ReadAll(resp.Body)
	// 用户要求：打印响应体片段（截断），确认设备实际返回内容（是否空数据/token 失效形态）
	c.log.Info("ubus: resp", "url", u, "session_id", sessionID, "obj", obj, "method", method,
		"status", resp.StatusCode, "body_head", head(dataBytes, 512))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.log.Warn("ubus: auth-status",
			"url", u, "session_id", sessionID, "obj", obj, "method", method, "status", resp.StatusCode)
		return nil, true, false, fmt.Errorf("ubus http %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Warn("ubus: non-200",
			"url", u, "session_id", sessionID, "obj", obj, "method", method,
			"status", resp.StatusCode, "body_head", head(dataBytes, 256))
		return nil, false, false, fmt.Errorf("ubus http %d: %s", resp.StatusCode, string(dataBytes))
	}
	var rr jsonrpcResponse
	if err := json.Unmarshal(dataBytes, &rr); err != nil {
		c.log.Warn("ubus: decode failed",
			"url", u, "session_id", sessionID, "obj", obj, "method", method,
			"err", err, "body_head", head(dataBytes, 256))
		return nil, false, false, fmt.Errorf("decode ubus response: %w (body head: %s)", err, head(dataBytes, 256))
	}
	if rr.Error != nil {
		// 协议层错误（如 -32002 Access denied：对象未安装/ACL 不允许；-32700 Parse error：请求体格式不符），重登无解，不重试
		c.log.Warn("ubus: jsonrpc error",
			"url", u, "session_id", sessionID, "obj", obj, "method", method,
			"code", rr.Error.Code, "message", rr.Error.Message, "body_head", head(dataBytes, 256))
		return nil, false, false, fmt.Errorf("ubus jsonrpc %d: %s", rr.Error.Code, rr.Error.Message)
	}
	// result: [code, data]
	var arr []json.RawMessage
	if err := json.Unmarshal(rr.Result, &arr); err != nil || len(arr) < 1 {
		c.log.Warn("ubus: invalid result",
			"url", u, "session_id", sessionID, "obj", obj, "method", method, "body_head", head(dataBytes, 256))
		return nil, false, false, fmt.Errorf("ubus: invalid result format")
	}
	var code int
	if err := json.Unmarshal(arr[0], &code); err != nil {
		return nil, false, false, fmt.Errorf("ubus: decode code: %w", err)
	}
	if code != 0 {
		// 6 = PERMISSION_DENIED：token 失效/会话过期，联动 lucirpc 失效后重试一次
		retry := code == 6
		c.log.Warn("ubus: device error",
			"url", u, "session_id", sessionID, "obj", obj, "method", method, "code", code, "retry", retry)
		return nil, retry, false, fmt.Errorf("ubus error code %d", code)
	}
	data1 := json.RawMessage("null")
	if len(arr) > 1 {
		data1 = arr[1]
	}
	empty := isEmptyData(data1)
	c.log.Debug("ubus: ok",
		"url", u, "session_id", sessionID, "obj", obj, "method", method, "result_len", len(data1), "empty", empty)
	return data1, false, empty, nil
}

// Call 调用 ubus：token 复用 lucirpc（自动 /auth 缓存），单设备并发限流，
// 遇 token 失效（result[0]=6 / 401/403）联动 lucirpc 失效并重试一次；
// 调用成功但无数据（empty）时按用户要求同样重取 token 再试一次，仍为空则按真实空数据返回。
// obj 是 ubus 对象名（如 "iwinfo"），method 是该对象的方法名（如 "assoclist"），
// params 是该方法参数对象（如 {"device":"phy0-ap0"}）。
func (c *Client) Call(ctx context.Context, sessionID, obj, method string, params map[string]any) (json.RawMessage, error) {
	release := c.acquire(sessionID)
	defer release()

	if c.rpc == nil {
		return nil, fmt.Errorf("ubus: no token source (lucirpc client is nil)")
	}
	// emptyRetried：空数据已重取过一次 token，避免"确实无数据"时无限重登
	emptyRetried := false
	for attempt := 0; attempt < 3; attempt++ {
		tok, err := c.rpc.Token(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		result, retry, empty, err := c.callUbus(ctx, sessionID, tok, 1, obj, method, params)
		if err == nil && !empty {
			return result, nil
		}
		if err == nil && empty {
			if emptyRetried {
				// 重取 token 后仍无数据：设备该接口/参数下确实无数据，按空结果返回
				c.log.Info("ubus: still empty after re-auth, treat as real empty",
					"session_id", sessionID, "obj", obj, "method", method)
				return result, nil
			}
			// 用户要求：无数据返回时视为疑似 token 失效，清缓存重新 /auth 再试一次
			c.log.Info("ubus: empty response, re-auth and retry",
				"session_id", sessionID, "obj", obj, "method", method, "attempt", attempt+1)
			emptyRetried = true
			c.rpc.Invalidate(sessionID)
			continue
		}
		if !retry {
			return nil, err
		}
		c.log.Info("ubus: retrying after token invalidate",
			"session_id", sessionID, "obj", obj, "method", method, "attempt", attempt+1)
		c.rpc.Invalidate(sessionID)
	}
	return nil, fmt.Errorf("ubus failed after retry: %s.%s", obj, method)
}

// CallInto 调用 ubus 并把 result[1] 解析到 out 指针。
func (c *Client) CallInto(ctx context.Context, sessionID, obj, method string, out any, params map[string]any) error {
	raw, err := c.Call(ctx, sessionID, obj, method, params)
	if err != nil {
		return err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// CallWrite 调用 ubus 写方法（uci.set/apply/commit/confirm/delete、network.reload 等）。
// 写方法成功时 result[1] 通常为空 {}——这是正常成功响应，不做"空数据→重取 token 重试"
// （该逻辑仅适用于读方法：实测 uci.apply 首次已提交成功，空响应被误判后重试第二次调用
// 因无暂存而报错，导致"实际已保存却报失败"，见 无线配置功能开发文档.md §3.3）。
// 仅在 result[0]=6（token 失效/会话过期）或 HTTP 401/403 时失效重登并重试一次。
func (c *Client) CallWrite(ctx context.Context, sessionID, obj, method string, params map[string]any) (json.RawMessage, error) {
	release := c.acquire(sessionID)
	defer release()

	if c.rpc == nil {
		return nil, fmt.Errorf("ubus: no token source (lucirpc client is nil)")
	}
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.rpc.Token(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		result, retry, _, err := c.callUbus(ctx, sessionID, tok, 1, obj, method, params)
		if err == nil {
			// 写方法：空响应（{} / null）即成功，直接返回
			return result, nil
		}
		if !retry {
			return nil, err
		}
		c.log.Info("ubus: write retrying after token invalidate",
			"session_id", sessionID, "obj", obj, "method", method, "attempt", attempt+1)
		c.rpc.Invalidate(sessionID)
	}
	return nil, fmt.Errorf("ubus write failed after retry: %s.%s", obj, method)
}

// CallWriteInto 调用 ubus 写方法并把 result[1] 解析到 out 指针（通常 out 传空 map 接收 {}）。
func (c *Client) CallWriteInto(ctx context.Context, sessionID, obj, method string, out any, params map[string]any) error {
	raw, err := c.CallWrite(ctx, sessionID, obj, method, params)
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

// isEmptyData 判断 result[1] 是否"无数据"（用户要求：无数据时重取 token 重试一次）。
// 视为空：null / 空串 / {} / [] / 仅含已知列表键且数组全为空（如 assoclist 空时返回 {"results":[]}）。
// 含非空业务数据（含标量字段的对象，如 iwinfo info 返回）不算空。
func isEmptyData(raw json.RawMessage) bool {
	s := string(raw)
	if len(raw) == 0 || s == "null" || s == "{}" || s == "[]" {
		return true
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		return len(arr) == 0
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false // 无法识别的结构，保守视为有数据
	}
	// 与 service.parseObjectList 保持一致的已知列表键
	listKeys := []string{"results", "stations", "items", "assoclist", "scanlist", "devices", "list"}
	hasListKey := false
	for _, key := range listKeys {
		v, ok := m[key]
		if !ok {
			continue
		}
		hasListKey = true
		var a []json.RawMessage
		if err := json.Unmarshal(v, &a); err == nil && len(a) > 0 {
			return false // 列表键存在且非空 → 有数据
		}
	}
	return hasListKey // 有列表键但全为空/非数组 → 空；无列表键的标量对象 → 有数据
}

// head 返回字节切片前 n 字节字符串（用于日志诊断，避免响应体过长淹没日志）。
func head(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
