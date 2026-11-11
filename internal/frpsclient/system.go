package frpsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ServiceStatusService 是 frps_helper 自身运行信息。
type ServiceStatusService struct {
	Name             string    `json:"name"`
	Version          string    `json:"version"`
	StartedAt        time.Time `json:"started_at"`
	UptimeSeconds    int64     `json:"uptime_seconds"`
	Goroutines       int       `json:"goroutines"`
	MemoryAllocBytes uint64    `json:"memory_alloc_bytes"`
	MemorySysBytes   uint64    `json:"memory_sys_bytes"`
}

// ServiceStatusDatabase 是 frps_helper 数据库连通状态。
type ServiceStatusDatabase struct {
	Type  string `json:"type"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// ServiceStatusRuntime 是 frps_helper 业务运行时汇总。
type ServiceStatusRuntime struct {
	OnlineDevices int                    `json:"online_devices"`
	ClientCount   int                    `json:"client_cnt"`
	Database      ServiceStatusDatabase  `json:"database"`
	Radius        map[string]interface{} `json:"radius"`
	AuthPort      int                    `json:"auth_port"`
	DevicePort    int                    `json:"device_port"`
	ManagePort    int                    `json:"manage_port"`
}

// FrpsStatusSummary 是 frps_helper 聚合的 FRPS dashboard 概况。
type FrpsStatusSummary struct {
	Reachable       bool   `json:"reachable"`
	Error           string `json:"error"`
	Version         string `json:"version"`
	BindPort        int    `json:"bind_port"`
	ClientCounts    int    `json:"client_counts"`
	OnlineClients   int    `json:"online_clients"`
	CurConns        int    `json:"cur_conns"`
	TotalTrafficIn  int64  `json:"total_traffic_in"`
	TotalTrafficOut int64  `json:"total_traffic_out"`
}

// SystemStatus 是 GET /api/v1/system/status 的 body。
type SystemStatus struct {
	Service ServiceStatusService `json:"service"`
	Runtime ServiceStatusRuntime `json:"runtime"`
	Frps    FrpsStatusSummary    `json:"frps"`
}

// GetSystemStatus 查询 frps_helper 运行状态（含 FRPS 概况聚合）。
func (c *Client) GetSystemStatus(ctx context.Context) (*SystemStatus, error) {
	data, err := c.get(ctx, "/api/v1/system/status", nil)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var st SystemStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, fmt.Errorf("decode system status: %w", err)
	}
	return &st, nil
}
