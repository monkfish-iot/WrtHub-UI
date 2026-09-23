package frpsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// GetCredential 查询设备明文登录凭证。
// 受 frps_helper 配置 frpc.credential_query=true 控制；关闭时返回 -1004 业务错误。
// 注意：返回值含明文密码，调用方须仅在内存中短缓存，不得落盘/输出日志/回传前端。
func (c *Client) GetCredential(ctx context.Context, deviceID string) (*Credential, error) {
	if deviceID == "" {
		return nil, &APIError{ErrorCode: -1004, Msg: "device_id is required"}
	}
	path := "/api/v1/credentials/" + url.PathEscape(deviceID)
	data, err := c.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var cred Credential
	if err := json.Unmarshal(body, &cred); err != nil {
		return nil, fmt.Errorf("decode credential: %w", err)
	}
	return &cred, nil
}
