package frpsclient

import (
	"encoding/json"
	"fmt"
	"time"
)

// Envelope 是北向接口统一响应信封 {error_code, msg, body}。
type Envelope struct {
	ErrorCode int             `json:"error_code"`
	Msg       string          `json:"msg"`
	Body      json.RawMessage `json:"body"`
}

// Device 是北向设备对象（password 永不回传，故不定义）。
type Device struct {
	ID               uint            `json:"id"`
	TenantOrgID      string          `json:"tenant_org_id"`
	DeviceID         string          `json:"device_id"`
	DeviceName       string          `json:"device_name"`
	Model            string          `json:"model"`
	Version          string          `json:"version"`
	MacAddress       string          `json:"mac_address"`
	Vendor           string          `json:"vendor"`
	Status           string          `json:"status"`
	AssignmentStatus string          `json:"assignment_status"`
	SessionID        string          `json:"session_id"`
	ExtIPAddress     string          `json:"ext_ip_address"`
	LanIP            string          `json:"lan_ip"`    // local_if 的 IPv4（join/status 上报）
	FrpcMode         string          `json:"frpc_mode"` // on=强制隧道，off=强制直连，""/auto=按 IP 自动判断
	UserName         string          `json:"user_name"`
	AdditionalData   json.RawMessage `json:"additional_data"`
	// RADIUS 对接参数（join 时由 frps_helper 填充；密钥字段在列表接口中隐藏，
	// 仅 GET /api/v1/devices/radius 透出，见 DeviceRadiusItem）
	RadAuthServer string     `json:"rad_auth_server"`
	RadAuthPort   int        `json:"rad_auth_port"`
	RadAcctServer string     `json:"rad_acct_server"`
	RadAcctPort   int        `json:"rad_acct_port"`
	RadNasIP      string     `json:"rad_nas_ip"`
	LastHeartbeat *time.Time `json:"last_heartbeat"`
	OnlineSince   *time.Time `json:"online_since"`
	FirstSeen     *time.Time `json:"first_seen"`
	CreatedAt     *time.Time `json:"created_at"`
	UpdatedAt     *time.Time `json:"updated_at"`
}

// DeviceListResult 是 GET /api/v1/devices 的 body。
type DeviceListResult struct {
	Total    int      `json:"total"`
	Page     int      `json:"page"`
	PageSize int      `json:"page_size"`
	Items    []Device `json:"items"`
}

// Credential 是 GET /api/v1/credentials/{device_id} 的 body。
type Credential struct {
	DeviceID string `json:"device_id"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// APIError 是北向业务错误（error_code != 0）。
type APIError struct {
	ErrorCode int
	Msg       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("frps_helper error %d: %s", e.ErrorCode, e.Msg)
}

// IsNotFound 判断是否"设备未找到"类业务错误。
func (e *APIError) IsNotFound() bool {
	return e.ErrorCode == -1004
}
