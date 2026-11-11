package frpsclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// Options 是 frps_helper 北向 client 的配置。
type Options struct {
	BaseURL     string        // 如 https://127.0.0.1:7443
	TLSInsecure bool          // 自签证书时跳过校验
	Timeout     time.Duration // 单请求超时
}

// Client 是 frps_helper 北向接口客户端。
// 超时以 atomic 存放、逐请求经 context 生效，支持 SetTimeout 运行时热更新。
type Client struct {
	http    *http.Client
	base    string
	timeout atomic.Int64 // 单请求超时（纳秒）
}

// New 构造 frps_helper 客户端。
func New(opts Options) *Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: opts.TLSInsecure},
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	c := &Client{
		http: &http.Client{Transport: tr},
		base: opts.BaseURL,
	}
	c.timeout.Store(int64(timeout))
	return c
}

// SetTimeout 热更新单请求超时（作用于后续请求；<=0 忽略）。
func (c *Client) SetTimeout(d time.Duration) {
	if d > 0 {
		c.timeout.Store(int64(d))
	}
}

func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	// 逐请求 context 超时（代替 http.Client.Timeout），配合 SetTimeout 热更新；
	// 响应体在本函数内读取完毕，defer cancel 不影响调用方。
	ctx, cancel := context.WithTimeout(req.Context(), time.Duration(c.timeout.Load()))
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("frps_helper http %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

// unwrap 解析统一信封，返回 body 字节；error_code != 0 返回 *APIError。
func unwrap(data []byte) ([]byte, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if env.ErrorCode != 0 {
		return nil, &APIError{ErrorCode: env.ErrorCode, Msg: env.Msg}
	}
	return env.Body, nil
}
