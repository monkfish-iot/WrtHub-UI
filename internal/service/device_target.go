package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"wrthub-ui/internal/frpsclient"
)

// DeviceTargetOptions 是设备访问地址解析配置。
type DeviceTargetOptions struct {
	Scheme       string        // 访问协议（http/https）
	DomainSuffix string        // 隧道域名后缀：{session_id}.{domain_suffix}
	TunnelPort   int           // frps 隧道端口（默认 20001）
	DirectPort   int           // 直连模式设备 nginx 端口（默认 8765）
	CacheTTL     time.Duration // 设备模式缓存 TTL（默认 1 分钟）
}

// DeviceTargetResolver 按 session_id 解析设备访问基地址：隧道模式走
// http://{session_id}.{domain}:{tunnel_port}，直连模式走 http://{lan_ip}:{direct_port}。
//
// 直连判定与 frps_helper 的 frpc_mode 语义保持一致：
//   - frpc_mode=on：强制隧道；
//   - frpc_mode=off：强制直连（需 lan_ip）；
//   - 空/auto：join 报文 local_if IP 等于客户端套接字 IP（即 ext_ip_address == lan_ip）时
//     服务端判定为直连、设备不下发隧道，此时必须直连设备 lan_ip。
//
// 北向查询失败时回退隧道地址，保持原有行为。
type DeviceTargetResolver struct {
	client *frpsclient.Client
	opts   DeviceTargetOptions
	log    *slog.Logger

	mu    sync.Mutex
	cache map[string]targetEntry
}

type targetEntry struct {
	host    string
	expires time.Time
}

// NewDeviceTargetResolver 构造设备访问地址解析器。
func NewDeviceTargetResolver(client *frpsclient.Client, opts DeviceTargetOptions, log *slog.Logger) *DeviceTargetResolver {
	if opts.Scheme == "" {
		opts.Scheme = "http"
	}
	if opts.DomainSuffix == "" {
		opts.DomainSuffix = "frpclient.local"
	}
	if opts.TunnelPort == 0 {
		opts.TunnelPort = 20001
	}
	if opts.DirectPort == 0 {
		opts.DirectPort = 8765
	}
	if opts.CacheTTL == 0 {
		opts.CacheTTL = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &DeviceTargetResolver{
		client: client,
		opts:   opts,
		log:    log,
		cache:  make(map[string]targetEntry),
	}
}

// Host 返回指定 session_id 的设备访问基地址。
func (r *DeviceTargetResolver) Host(ctx context.Context, sessionID string) (string, error) {
	tunnel := r.tunnelHost(sessionID)
	if sessionID == "" {
		return tunnel, nil
	}

	r.mu.Lock()
	if e, ok := r.cache[sessionID]; ok && time.Now().Before(e.expires) {
		host := e.host
		r.mu.Unlock()
		return host, nil
	}
	r.mu.Unlock()

	host := tunnel
	dev, err := r.client.GetDeviceBySession(ctx, sessionID)
	if err != nil {
		r.log.Warn("device target: resolve by session failed, fallback to tunnel",
			"session_id", sessionID, "err", err)
	} else if directDevice(dev) && dev.LanIP != "" {
		host = r.directHost(dev.LanIP)
		r.log.Info("device target: direct mode", "session_id", sessionID, "lan_ip", dev.LanIP)
	}

	r.mu.Lock()
	r.cache[sessionID] = targetEntry{host: host, expires: time.Now().Add(r.opts.CacheTTL)}
	r.mu.Unlock()
	return host, nil
}

func (r *DeviceTargetResolver) tunnelHost(sessionID string) string {
	return fmt.Sprintf("%s://%s.%s:%d", r.opts.Scheme, sessionID, r.opts.DomainSuffix, r.opts.TunnelPort)
}

func (r *DeviceTargetResolver) directHost(lanIP string) string {
	return fmt.Sprintf("%s://%s:%d", r.opts.Scheme, lanIP, r.opts.DirectPort)
}

// directDevice 判断设备是否处于直连模式（不建 frp 隧道）。
func directDevice(dev *frpsclient.Device) bool {
	if dev == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(dev.FrpcMode)) {
	case "on":
		return false
	case "off":
		return true
	default:
		// auto：frps_helper 在 local_if IP 与套接字 IP 一致时下发 frpc_mode=0
		return dev.LanIP != "" && dev.ExtIPAddress == dev.LanIP
	}
}
