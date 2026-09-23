// Package service 实现跨包业务编排层：凭证映射、设备代理、无线管理等。
//
// 本包不持有任何 HTTP 处理逻辑，仅向 handlers 层提供面向业务的 Go API。
package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"wrthub-ui/internal/frpsclient"
)

// CredentialProvider 实现 lucirpc.CredentialProvider：按 session_id 取设备登录凭证。
//
// 安全约束（见开发技术文档 §4.3）：
//   - 明文密码仅服务端内存短缓存，TTL 默认 5 分钟；
//   - 不落盘、不进日志、不回传前端；
//   - session_id → device_id 映射缓存 TTL 较长（默认 30 分钟），不含敏感数据。
type CredentialProvider struct {
	client *frpsclient.Client

	mapTTL time.Duration // session_id → device_id 映射缓存 TTL
	credTTL time.Duration // 凭证缓存 TTL（必须短）

	mu       sync.Mutex
	idMap    map[string]idEntry   // session_id → device_id
	credMap  map[string]credEntry // device_id → 凭证
}

type idEntry struct {
	deviceID string
	expires time.Time
}

type credEntry struct {
	username, password string
	expires             time.Time
}

// NewCredentialProvider 构造凭证提供者。
// mapTTL/credTTL 为 0 时分别取 30m / 5m。
func NewCredentialProvider(client *frpsclient.Client, mapTTL, credTTL time.Duration) *CredentialProvider {
	if mapTTL == 0 {
		mapTTL = 30 * time.Minute
	}
	if credTTL == 0 {
		credTTL = 5 * time.Minute
	}
	return &CredentialProvider{
		client:  client,
		mapTTL:  mapTTL,
		credTTL: credTTL,
		idMap:   make(map[string]idEntry),
		credMap: make(map[string]credEntry),
	}
}

// Credential 按 session_id 返回设备登录凭证。
// 内部流程：查 session_id→device_id 映射 → 命中则查 device_id→凭证缓存 → 未命中则查 frps_helper。
func (p *CredentialProvider) Credential(ctx context.Context, sessionID string) (username, password string, err error) {
	deviceID, err := p.deviceID(ctx, sessionID)
	if err != nil {
		return "", "", err
	}

	p.mu.Lock()
	if ce, ok := p.credMap[deviceID]; ok && time.Now().Before(ce.expires) {
		u, pwd := ce.username, ce.password
		p.mu.Unlock()
		return u, pwd, nil
	}
	p.mu.Unlock()

	cred, err := p.client.GetCredential(ctx, deviceID)
	if err != nil {
		return "", "", fmt.Errorf("get credential: %w", err)
	}

	p.mu.Lock()
	p.credMap[deviceID] = credEntry{username: cred.Username, password: cred.Password, expires: time.Now().Add(p.credTTL)}
	p.mu.Unlock()
	return cred.Username, cred.Password, nil
}

// deviceID 解析 session_id → device_id，优先命中映射缓存。
func (p *CredentialProvider) deviceID(ctx context.Context, sessionID string) (string, error) {
	p.mu.Lock()
	if e, ok := p.idMap[sessionID]; ok && time.Now().Before(e.expires) {
		id := e.deviceID
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()

	dev, err := p.client.GetDeviceBySession(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("resolve device_id by session: %w", err)
	}

	p.mu.Lock()
	p.idMap[sessionID] = idEntry{deviceID: dev.DeviceID, expires: time.Now().Add(p.mapTTL)}
	p.mu.Unlock()
	return dev.DeviceID, nil
}

// Invalidate 清除指定 session_id 的映射与凭证缓存（设备下线或鉴权持续失败时调用）。
func (p *CredentialProvider) Invalidate(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.idMap[sessionID]; ok {
		delete(p.idMap, sessionID)
		delete(p.credMap, e.deviceID)
	}
}
