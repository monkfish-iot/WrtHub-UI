package ubusrpc

import "encoding/json"

// jsonrpcRequest 是 /ubus 端点 JSON-RPC 2.0 请求体。
//
// 必须带 "jsonrpc":"2.0" 字段：实测设备端（OpenWrt 25.12.2）缺该字段时返回
// -32700 Parse error（即使 body 是合法 JSON），与手工 curl 成功示例保持一致。
//
// ubus jsonrpc 固定 method="call"，params 长度 4：
//
//	["<token>", "<obj>", "<method>", {<params_obj>}]
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"` // 固定 "2.0"
	ID      int    `json:"id"`
	Method  string `json:"method"` // 固定 "call"
	Params  []any  `json:"params"`
}

// jsonrpcResponse 是 /ubus 端点响应。
//
// 成功：{"jsonrpc":"2.0","id":1,"result":[0, <data>]}
// 失败：{"jsonrpc":"2.0","id":1,"result":[<errno>, null]}
// 协议层错误：{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"..."}}
//
// ubus 约定 result[0]=0 表成功；非 0 为 ubus 错误码（如 6 = 未授权/会话失效），
// result[1] 为业务数据（错误时通常为 null）。
type jsonrpcResponse struct {
	ID     json.Number     `json:"id"`
	Result json.RawMessage `json:"result"` // 长度 2 数组 [code, data]
	Error  *jsonrpcError   `json:"error"`  // 协议层错误（非空表示请求格式/传输错误）
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
