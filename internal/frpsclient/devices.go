package frpsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// ListOptions 是 GET /api/v1/devices 的查询参数（均为可选，AND 组合，LIKE 模糊）。
type ListOptions struct {
	Page     int
	PageSize int // 上限 100，超限由服务端截断
	Status   string
	DeviceID string
	Mac      string
	Model    string
	Keyword  string // 同时匹配 device_id/device_name/mac_address
}

func (o ListOptions) query() url.Values {
	q := url.Values{}
	if o.Page > 0 {
		q.Set("page", strconv.Itoa(o.Page))
	}
	if o.PageSize > 0 {
		q.Set("page_size", strconv.Itoa(o.PageSize))
	}
	if o.Status != "" {
		q.Set("status", o.Status)
	}
	if o.DeviceID != "" {
		q.Set("device_id", o.DeviceID)
	}
	if o.Mac != "" {
		q.Set("mac", o.Mac)
	}
	if o.Model != "" {
		q.Set("model", o.Model)
	}
	if o.Keyword != "" {
		q.Set("keyword", o.Keyword)
	}
	return q
}

// ListDevices 分页查询设备列表。
func (c *Client) ListDevices(ctx context.Context, opts ListOptions) (*DeviceListResult, error) {
	data, err := c.get(ctx, "/api/v1/devices", opts.query())
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var r DeviceListResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode device list: %w", err)
	}
	return &r, nil
}

// GetDevice 按 device_id 获取单个设备。北向无单设备接口，用 device_id 过滤取第一条。
func (c *Client) GetDevice(ctx context.Context, deviceID string) (*Device, error) {
	r, err := c.ListDevices(ctx, ListOptions{DeviceID: deviceID, PageSize: 1})
	if err != nil {
		return nil, err
	}
	if len(r.Items) == 0 {
		return nil, &APIError{ErrorCode: -1004, Msg: "device not found"}
	}
	return &r.Items[0], nil
}

// GetDeviceBySession 按 session_id 获取单个设备。
// 路径风格与凭证接口一致：GET /api/v1/devices/session/{session_id}，
// 服务端精确匹配 session_id，未找到返回业务错误 -1004。
func (c *Client) GetDeviceBySession(ctx context.Context, sessionID string) (*Device, error) {
	if sessionID == "" {
		return nil, &APIError{ErrorCode: -1004, Msg: "session_id is required"}
	}
	data, err := c.get(ctx, "/api/v1/devices/session/"+url.PathEscape(sessionID), nil)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var dev Device
	if err := json.Unmarshal(body, &dev); err != nil {
		return nil, fmt.Errorf("decode device: %w", err)
	}
	return &dev, nil
}
