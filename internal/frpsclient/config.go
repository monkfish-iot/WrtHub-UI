package frpsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ConfigSnapshot 是北向配置快照对象（列表项不含 content）。
type ConfigSnapshot struct {
	ID           uint      `json:"id"`
	TenantOrgID  string    `json:"tenant_org_id"`
	DeviceID     string    `json:"device_id"`
	SnapshotID   string    `json:"snapshot_id"`
	Content      string    `json:"content,omitempty"`
	SHA256       string    `json:"sha256"`
	Size         int       `json:"size"`
	PackageCount int       `json:"package_count"`
	Firmware     string    `json:"firmware"`
	Model        string    `json:"model"`
	Source       string    `json:"source"`
	Status       string    `json:"status"`
	CapturedAt   time.Time `json:"captured_at"`
	CreatedAt    time.Time `json:"created_at"`
}

// ConfigSnapshotListResult 是快照列表 body。
type ConfigSnapshotListResult struct {
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Items    []ConfigSnapshot `json:"items"`
}

// CreateConfigSnapshotRequest 是创建快照请求。
type CreateConfigSnapshotRequest struct {
	TenantOrgID  string `json:"tenant_org_id"`
	DeviceID     string `json:"device_id"`
	Content      string `json:"content"`
	SHA256       string `json:"sha256"`
	Size         int    `json:"size"`
	PackageCount int    `json:"package_count"`
	Firmware     string `json:"firmware"`
	Model        string `json:"model"`
	Source       string `json:"source"`
	CapturedAt   string `json:"captured_at"`
}

// CreateConfigSnapshotResult 是创建快照响应（sha256 命中时 deduplicated=true）。
type CreateConfigSnapshotResult struct {
	Deduplicated bool           `json:"deduplicated"`
	Snapshot     ConfigSnapshot `json:"snapshot"`
}

// ConfigItem 是关键项状态对象。
type ConfigItem struct {
	ID            uint       `json:"id"`
	DeviceID      string     `json:"device_id"`
	ItemKey       string     `json:"item_key"`
	Value         string     `json:"value"`
	ExpectedValue string     `json:"expected_value"`
	Status        string     `json:"status"`
	CheckedAt     *time.Time `json:"checked_at"`
	RepairedAt    *time.Time `json:"repaired_at"`
	LastError     string     `json:"last_error"`
}

// ConfigPolicy 是设备配置管理策略。
type ConfigPolicy struct {
	ID              uint   `json:"id"`
	TenantOrgID     string `json:"tenant_org_id"`
	DeviceID        string `json:"device_id"`
	SnapshotEnabled int    `json:"snapshot_enabled"`
	CheckEnabled    int    `json:"check_enabled"`
	AutoRepair      int    `json:"auto_repair"`
	CheckInterval   int    `json:"check_interval"`
	RetainSnapshots int    `json:"retain_snapshots"`
}

func (c *Client) post(ctx context.Context, path string, payload any) ([]byte, error) {
	return c.send(ctx, http.MethodPost, path, payload)
}

func (c *Client) put(ctx context.Context, path string, payload any) ([]byte, error) {
	return c.send(ctx, http.MethodPut, path, payload)
}

func (c *Client) send(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

// ListConfigSnapshots 分页查询设备快照（items 不含 content）。
func (c *Client) ListConfigSnapshots(ctx context.Context, deviceID string, page, pageSize int) (*ConfigSnapshotListResult, error) {
	q := url.Values{}
	q.Set("device_id", deviceID)
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	if pageSize > 0 {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	data, err := c.get(ctx, "/api/v1/config/snapshots", q)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var r ConfigSnapshotListResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode snapshot list: %w", err)
	}
	return &r, nil
}

// GetConfigSnapshot 获取快照详情（含 content）。
func (c *Client) GetConfigSnapshot(ctx context.Context, snapshotID string) (*ConfigSnapshot, error) {
	data, err := c.get(ctx, "/api/v1/config/snapshots/"+url.PathEscape(snapshotID), nil)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var snap ConfigSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return &snap, nil
}

// CreateConfigSnapshot 创建快照（sha256 相同则复用）。
func (c *Client) CreateConfigSnapshot(ctx context.Context, req *CreateConfigSnapshotRequest) (*CreateConfigSnapshotResult, error) {
	data, err := c.post(ctx, "/api/v1/config/snapshots", req)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var r CreateConfigSnapshotResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode create snapshot: %w", err)
	}
	return &r, nil
}

// DeleteConfigSnapshot 删除快照。
func (c *Client) DeleteConfigSnapshot(ctx context.Context, snapshotID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.base+"/api/v1/config/snapshots/"+url.PathEscape(snapshotID), nil)
	if err != nil {
		return err
	}
	data, err := c.do(req)
	if err != nil {
		return err
	}
	_, err = unwrap(data)
	return err
}

// GetConfigPolicy 读取设备策略（无记录返回默认值）。
func (c *Client) GetConfigPolicy(ctx context.Context, deviceID string) (*ConfigPolicy, error) {
	data, err := c.get(ctx, "/api/v1/config/policy/"+url.PathEscape(deviceID), nil)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var p ConfigPolicy
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decode policy: %w", err)
	}
	return &p, nil
}

// PutConfigPolicy 更新设备策略。
func (c *Client) PutConfigPolicy(ctx context.Context, deviceID string, p *ConfigPolicy) (*ConfigPolicy, error) {
	data, err := c.put(ctx, "/api/v1/config/policy/"+url.PathEscape(deviceID), p)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var out ConfigPolicy
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode policy: %w", err)
	}
	return &out, nil
}

// ListConfigItems 查询设备关键项状态。
func (c *Client) ListConfigItems(ctx context.Context, deviceID string) ([]ConfigItem, error) {
	q := url.Values{}
	q.Set("device_id", deviceID)
	data, err := c.get(ctx, "/api/v1/config/items", q)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var r struct {
		Total int          `json:"total"`
		Items []ConfigItem `json:"items"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode items: %w", err)
	}
	return r.Items, nil
}

// UpsertConfigItems 批量写入关键项状态。
func (c *Client) UpsertConfigItems(ctx context.Context, deviceID, tenantOrgID string, items []ConfigItem) error {
	payload := map[string]any{
		"device_id":     deviceID,
		"tenant_org_id": tenantOrgID,
		"items":         items,
	}
	data, err := c.put(ctx, "/api/v1/config/items", payload)
	if err != nil {
		return err
	}
	_, err = unwrap(data)
	return err
}
