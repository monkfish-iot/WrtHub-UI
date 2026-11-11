package frpsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// RadiusGlobalConfig 是全局 RADIUS 配置（GET/PUT /api/v1/config/radius 的 body）。
// effective_* 为服务端按回退规则计算后的生效值；runtime 为 RADIUS 服务运行状态。
type RadiusGlobalConfig struct {
	AuthEnable bool `json:"auth_enable"`
	AcctEnable bool `json:"acct_enable"`

	AuthListenAddr string `json:"auth_listen_addr"`
	AuthListenPort int    `json:"auth_listen_port"`
	AcctListenAddr string `json:"acct_listen_addr"`
	AcctListenPort int    `json:"acct_listen_port"`

	AuthSecret string `json:"auth_secret"`
	AcctSecret string `json:"acct_secret"`

	AuthTimeout         int `json:"auth_timeout"`
	AcctTimeout         int `json:"acct_timeout"`
	AcctSessionTimeout  int `json:"acct_session_timeout"`
	AcctCleanupInterval int `json:"acct_cleanup_interval"`

	AdvertisedServer   string `json:"advertised_server"`
	AdvertisedAuthPort int    `json:"advertised_auth_port"`
	AdvertisedAcctPort int    `json:"advertised_acct_port"`

	// 认证源：local（默认，本地设备凭证）| radius | ldap | http
	AuthSource string `json:"auth_source"`

	// 上游 RADIUS 代理转发（auth_source=radius）
	UpstreamServer        string `json:"upstream_server"`
	UpstreamAuthPort      int    `json:"upstream_auth_port"`
	UpstreamAcctPort      int    `json:"upstream_acct_port"`
	UpstreamSecret        string `json:"upstream_secret"`
	UpstreamStopOnTimeout bool   `json:"upstream_stop_on_timeout"`

	// LDAP/AD（auth_source=ldap）
	LDAPServer       string `json:"ldap_server"`
	LDAPPort         int    `json:"ldap_port"`
	LDAPBindDN       string `json:"ldap_bind_dn"`
	LDAPBindPassword string `json:"ldap_bind_password"`
	LDAPBaseDN       string `json:"ldap_base_dn"`
	LDAPUserFilter   string `json:"ldap_user_filter"`
	LDAPTimeout      int    `json:"ldap_timeout"`
	LDAPUseTLS       bool   `json:"ldap_use_tls"`

	// HTTP(S)（auth_source=http）
	HTTPAuthURL      string `json:"http_auth_url"`
	HTTPAcctURL      string `json:"http_acct_url"`
	HTTPPasswordType string `json:"http_password_type"`
	HTTPSharedSecret string `json:"http_shared_secret"`
	HTTPTimeout      int    `json:"http_timeout"`
	HTTPTLSVerify    bool   `json:"http_tls_verify"`

	// 转发时强制写入的 NAS-IP-Address（非空时替换/追加报文中的属性 4）
	NASIP string `json:"nas_ip"`

	EffectiveServer   string                 `json:"effective_server"`
	EffectiveAuthPort int                    `json:"effective_auth_port"`
	EffectiveAcctPort int                    `json:"effective_acct_port"`
	Runtime           map[string]interface{} `json:"runtime"`
}

// GetRadiusConfig 查询全局 RADIUS 配置。
func (c *Client) GetRadiusConfig(ctx context.Context) (*RadiusGlobalConfig, error) {
	data, err := c.get(ctx, "/api/v1/config/radius", nil)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var cfg RadiusGlobalConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("decode radius config: %w", err)
	}
	return &cfg, nil
}

// UpdateRadiusConfig 更新全局 RADIUS 配置（服务端持久化 + 运行时生效）。
func (c *Client) UpdateRadiusConfig(ctx context.Context, cfg *RadiusGlobalConfig) (*RadiusGlobalConfig, error) {
	// effective_/runtime 为响应字段，更新请求不回传
	req := map[string]interface{}{
		"auth_enable":              cfg.AuthEnable,
		"acct_enable":              cfg.AcctEnable,
		"auth_listen_addr":         cfg.AuthListenAddr,
		"auth_listen_port":         cfg.AuthListenPort,
		"acct_listen_addr":         cfg.AcctListenAddr,
		"acct_listen_port":         cfg.AcctListenPort,
		"auth_secret":              cfg.AuthSecret,
		"acct_secret":              cfg.AcctSecret,
		"auth_timeout":             cfg.AuthTimeout,
		"acct_timeout":             cfg.AcctTimeout,
		"acct_session_timeout":     cfg.AcctSessionTimeout,
		"acct_cleanup_interval":    cfg.AcctCleanupInterval,
		"advertised_server":        cfg.AdvertisedServer,
		"advertised_auth_port":     cfg.AdvertisedAuthPort,
		"advertised_acct_port":     cfg.AdvertisedAcctPort,
		"auth_source":              cfg.AuthSource,
		"upstream_server":          cfg.UpstreamServer,
		"upstream_auth_port":       cfg.UpstreamAuthPort,
		"upstream_acct_port":       cfg.UpstreamAcctPort,
		"upstream_secret":          cfg.UpstreamSecret,
		"upstream_stop_on_timeout": cfg.UpstreamStopOnTimeout,
		"ldap_server":              cfg.LDAPServer,
		"ldap_port":                cfg.LDAPPort,
		"ldap_bind_dn":             cfg.LDAPBindDN,
		"ldap_bind_password":       cfg.LDAPBindPassword,
		"ldap_base_dn":             cfg.LDAPBaseDN,
		"ldap_user_filter":         cfg.LDAPUserFilter,
		"ldap_timeout":             cfg.LDAPTimeout,
		"ldap_use_tls":             cfg.LDAPUseTLS,
		"http_auth_url":            cfg.HTTPAuthURL,
		"http_acct_url":            cfg.HTTPAcctURL,
		"http_password_type":       cfg.HTTPPasswordType,
		"http_shared_secret":       cfg.HTTPSharedSecret,
		"http_timeout":             cfg.HTTPTimeout,
		"http_tls_verify":          cfg.HTTPTLSVerify,
		"nas_ip":                   cfg.NASIP,
	}
	data, err := c.put(ctx, "/api/v1/config/radius", req)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var out RadiusGlobalConfig
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode radius config: %w", err)
	}
	return &out, nil
}

// UpdateRadiusAuthSource 只更新认证源及其参数。
// 认证源管理页不维护服务监听/密钥/超时/宣告参数，若整份配置回传，
// 未配置的 auth_listen_addr 等零值会被 frps_helper 判为非法；
// 故这里只发送本页负责的字段（指针语义由服务端处理）。
func (c *Client) UpdateRadiusAuthSource(ctx context.Context, cfg *RadiusGlobalConfig) (*RadiusGlobalConfig, error) {
	req := map[string]interface{}{
		"auth_source":              cfg.AuthSource,
		"upstream_server":          cfg.UpstreamServer,
		"upstream_auth_port":       cfg.UpstreamAuthPort,
		"upstream_acct_port":       cfg.UpstreamAcctPort,
		"upstream_secret":          cfg.UpstreamSecret,
		"upstream_stop_on_timeout": cfg.UpstreamStopOnTimeout,
		"ldap_server":              cfg.LDAPServer,
		"ldap_port":                cfg.LDAPPort,
		"ldap_bind_dn":             cfg.LDAPBindDN,
		"ldap_bind_password":       cfg.LDAPBindPassword,
		"ldap_base_dn":             cfg.LDAPBaseDN,
		"ldap_user_filter":         cfg.LDAPUserFilter,
		"ldap_timeout":             cfg.LDAPTimeout,
		"ldap_use_tls":             cfg.LDAPUseTLS,
		"http_auth_url":            cfg.HTTPAuthURL,
		"http_acct_url":            cfg.HTTPAcctURL,
		"http_password_type":       cfg.HTTPPasswordType,
		"http_shared_secret":       cfg.HTTPSharedSecret,
		"http_timeout":             cfg.HTTPTimeout,
		"http_tls_verify":          cfg.HTTPTLSVerify,
		"nas_ip":                   cfg.NASIP,
	}
	data, err := c.put(ctx, "/api/v1/config/radius", req)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var out RadiusGlobalConfig
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode radius config: %w", err)
	}
	return &out, nil
}

// RadiusSessionItem 是当前活跃记账会话（GET /api/v1/radius/sessions 的 items）。
// acct_* 为 NAS 最新上报的累计值；start/update 为本地记录时间。
type RadiusSessionItem struct {
	AcctSessionID    string    `json:"acct_session_id"`
	NasIdentifier    string    `json:"nas_identifier"`
	Username         string    `json:"username"`
	CallingStationID string    `json:"calling_station_id"`
	CalledStationID  string    `json:"called_station_id"`
	FramedIPAddress  string    `json:"framed_ip_address"`
	AcctSessionTime  int64     `json:"acct_session_time"`
	AcctInputOctets  int64     `json:"acct_input_octets"`
	AcctOutputOctets int64     `json:"acct_output_octets"`
	AcctStartTime    time.Time `json:"acct_start_time"`
	AcctUpdateTime   time.Time `json:"acct_update_time"`
}

// RadiusSessionListResult 是 GET /api/v1/radius/sessions 的 body。
type RadiusSessionListResult struct {
	Total    int                 `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
	Items    []RadiusSessionItem `json:"items"`
}

// ListRadiusSessions 分页查询当前活跃记账会话（keyword 模糊匹配用户名/Session-Id/NAS 标识）。
func (c *Client) ListRadiusSessions(ctx context.Context, page, pageSize int, keyword string) (*RadiusSessionListResult, error) {
	q := url.Values{}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	if pageSize > 0 {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	if keyword != "" {
		q.Set("keyword", keyword)
	}
	data, err := c.get(ctx, "/api/v1/radius/sessions", q)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var r RadiusSessionListResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode radius sessions: %w", err)
	}
	return &r, nil
}

// DeviceRadiusItem 是单台设备的 RADIUS 对接参数（含密钥，仅管理接口透出）。
type DeviceRadiusItem struct {
	DeviceID      string `json:"device_id"`
	RadAuthServer string `json:"rad_auth_server"`
	RadAuthPort   int    `json:"rad_auth_port"`
	RadAuthSecret string `json:"rad_auth_secret"`
	RadAcctServer string `json:"rad_acct_server"`
	RadAcctPort   int    `json:"rad_acct_port"`
	RadAcctSecret string `json:"rad_acct_secret"`
	RadNasIP      string `json:"rad_nas_ip"`
}

// DeviceRadiusListResult 是 GET /api/v1/devices/radius 的 body。
type DeviceRadiusListResult struct {
	Total    int                `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
	Items    []DeviceRadiusItem `json:"items"`
}

// ListDeviceRadius 分页查询每设备 RADIUS 参数（keyword 模糊匹配 ID/名称/MAC）。
func (c *Client) ListDeviceRadius(ctx context.Context, page, pageSize int, keyword string) (*DeviceRadiusListResult, error) {
	q := url.Values{}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	if pageSize > 0 {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	if keyword != "" {
		q.Set("keyword", keyword)
	}
	data, err := c.get(ctx, "/api/v1/devices/radius", q)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var r DeviceRadiusListResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode device radius list: %w", err)
	}
	return &r, nil
}

// DeviceRadiusUpdate 是编辑每设备 RADIUS 参数的请求体。
// 服务端为指针语义：nil 不修改；空串/0 显式清空（清空后设备重新 join 会再填充）。
type DeviceRadiusUpdate struct {
	RadAuthServer *string `json:"rad_auth_server,omitempty"`
	RadAcctServer *string `json:"rad_acct_server,omitempty"`
	RadNasIP      *string `json:"rad_nas_ip,omitempty"`
	RadAuthSecret *string `json:"rad_auth_secret,omitempty"`
	RadAcctSecret *string `json:"rad_acct_secret,omitempty"`
	RadAuthPort   *int    `json:"rad_auth_port,omitempty"`
	RadAcctPort   *int    `json:"rad_acct_port,omitempty"`
}

// UpdateDeviceRadius 编辑单台设备的 RADIUS 参数。
func (c *Client) UpdateDeviceRadius(ctx context.Context, deviceID string, upd *DeviceRadiusUpdate) (*DeviceRadiusItem, error) {
	data, err := c.put(ctx, "/api/v1/devices/radius/"+url.PathEscape(deviceID), upd)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var item DeviceRadiusItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("decode device radius: %w", err)
	}
	return &item, nil
}

// ---------- 本地 RADIUS 用户 ----------

// LocalUserItem 本地 RADIUS 用户（auth_source=local 时的认证数据源）。
type LocalUserItem struct {
	ID          uint   `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"` // active | disabled
	Remark      string `json:"remark"`

	// 网络策略（认证成功时随 Access-Accept 下发；0 = 不限）
	DownRateMbps      uint32 `json:"down_rate_mbps"`
	UpRateMbps        uint32 `json:"up_rate_mbps"`
	DataQuotaMB       uint32 `json:"data_quota_mb"`
	SessionTimeoutSec uint32 `json:"session_timeout_sec"`
	IdleTimeoutSec    uint32 `json:"idle_timeout_sec"`

	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	LastLoginIP string     `json:"last_login_ip"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// LocalUserListResult 是 GET /api/v1/local-users 的 body。
type LocalUserListResult struct {
	Total    int             `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"size"`
	List     []LocalUserItem `json:"list"`
}

// ListLocalUsers 分页查询本地 RADIUS 用户。
func (c *Client) ListLocalUsers(ctx context.Context, page, size int, q, status string) (*LocalUserListResult, error) {
	query := url.Values{}
	if page > 0 {
		query.Set("page", strconv.Itoa(page))
	}
	if size > 0 {
		query.Set("size", strconv.Itoa(size))
	}
	if q != "" {
		query.Set("q", q)
	}
	if status != "" {
		query.Set("status", status)
	}
	data, err := c.get(ctx, "/api/v1/local-users", query)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var r LocalUserListResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode local users: %w", err)
	}
	return &r, nil
}

// LocalUserCreateReq 创建本地用户请求。
type LocalUserCreateReq struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	Remark      string `json:"remark"`

	// 网络策略（0 = 不限）
	DownRateMbps      uint32 `json:"down_rate_mbps"`
	UpRateMbps        uint32 `json:"up_rate_mbps"`
	DataQuotaMB       uint32 `json:"data_quota_mb"`
	SessionTimeoutSec uint32 `json:"session_timeout_sec"`
	IdleTimeoutSec    uint32 `json:"idle_timeout_sec"`
}

// CreateLocalUser 创建本地 RADIUS 用户。
func (c *Client) CreateLocalUser(ctx context.Context, req *LocalUserCreateReq) (*LocalUserItem, error) {
	data, err := c.post(ctx, "/api/v1/local-users", req)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var item LocalUserItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("decode local user: %w", err)
	}
	return &item, nil
}

// LocalUserUpdateReq 更新本地用户请求；字符串字段为空或不传不修改。
// 网络策略字段整组覆盖（不带 omitempty）：0 是合法值，代表重置为不限。
type LocalUserUpdateReq struct {
	Password    string `json:"password,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Status      string `json:"status,omitempty"`
	Remark      string `json:"remark,omitempty"`

	DownRateMbps      uint32 `json:"down_rate_mbps"`
	UpRateMbps        uint32 `json:"up_rate_mbps"`
	DataQuotaMB       uint32 `json:"data_quota_mb"`
	SessionTimeoutSec uint32 `json:"session_timeout_sec"`
	IdleTimeoutSec    uint32 `json:"idle_timeout_sec"`
}

// GetLocalUser 查询单个本地用户详情（编辑预填使用）。
func (c *Client) GetLocalUser(ctx context.Context, id uint) (*LocalUserItem, error) {
	data, err := c.get(ctx, fmt.Sprintf("/api/v1/local-users/%d", id), nil)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var item LocalUserItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("decode local user: %w", err)
	}
	return &item, nil
}

// UpdateLocalUser 更新本地 RADIUS 用户。
func (c *Client) UpdateLocalUser(ctx context.Context, id uint, req *LocalUserUpdateReq) error {
	_, err := c.put(ctx, fmt.Sprintf("/api/v1/local-users/%d", id), req)
	return err
}

// DeleteLocalUser 删除本地 RADIUS 用户。
func (c *Client) DeleteLocalUser(ctx context.Context, id uint) error {
	data, err := c.send(ctx, http.MethodDelete, fmt.Sprintf("/api/v1/local-users/%d", id), nil)
	if err != nil {
		return err
	}
	_, err = unwrap(data)
	return err
}

// LocalUserLoginLogItem 单条本地用户登录日志。
type LocalUserLoginLogItem struct {
	ID        uint      `json:"id"`
	Username  string    `json:"username"`
	Success   bool      `json:"success"`
	SourceIP  string    `json:"source_ip"`
	NASIP     string    `json:"nas_ip"`
	NASID     string    `json:"nas_id"`
	Msg       string    `json:"msg"`
	CreatedAt time.Time `json:"created_at"`
}

// LocalUserLogsResult 是 GET /api/v1/local-users/{id}/login-logs 的 body。
type LocalUserLogsResult struct {
	Username string                  `json:"username"`
	Logs     []LocalUserLoginLogItem `json:"logs"`
}

// ListLocalUserLogs 查询某本地用户的登录日志（默认 50 条）。
func (c *Client) ListLocalUserLogs(ctx context.Context, id uint, limit int) (*LocalUserLogsResult, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := fmt.Sprintf("/api/v1/local-users/%d/login-logs", id)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	data, err := c.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var r LocalUserLogsResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode local user logs: %w", err)
	}
	return &r, nil
}
