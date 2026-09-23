package lucirpc

import "encoding/json"

// authRequest 是 POST /auth 的请求体。
type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// authResponse 是 POST /auth 的响应。
//
// 实际响应格式（设备实测示例）：
//
//	{"approved":true,"auth_token":"<frps内部token>","luci_token":"<RPC_TOKEN>","username":""}
//
// 字段说明：
//   - approved:    是否批准登录；false 表示用户名/密码错
//   - auth_token:  frps_helper 内部用的 token（本系统不使用）
//   - luci_token:  LuCI RPC 调用凭据，作为 ?auth= 参数传给 /cgi-bin/luci/rpc/*
//   - username:    设备回显的用户名（实测常为空串）
//
// 注意：这不是标准 LuCI RPC 文档里的 {"id":1,"result":"<token>","error":null}
// 格式，而是 frps_helper 改造后的 /auth 端点。以设备实测为准。
type authResponse struct {
	Approved  bool   `json:"approved"`
	AuthToken string `json:"auth_token"`
	LuciToken string `json:"luci_token"`
	Username  string `json:"username"`
}

// rpcRequest 是 /cgi-bin/luci/rpc/{ns}?auth= 的请求体。
// id 字段可选（设备实测请求不带 id 时响应 id 为 null，不影响 result 解析）。
type rpcRequest struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Params []any  `json:"params"`
}

// rpcResponse 是 RPC 调用响应。
// 成功：{"id":<num或null>,"result":<data>,"error":null}
// 失败：{"id":null,"result":null,"error":{"message":"...","code":-32602,"data":"..."}}
type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  any             `json:"error"` // 成功为 null，失败为 object（含 message/code/data）
}
