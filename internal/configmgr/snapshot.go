package configmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// UbusCaller 抽象 ubus 调用（*ubusrpc.Client 满足该接口），便于测试。
type UbusCaller interface {
	CallInto(ctx context.Context, sessionID, obj, method string, out any, params map[string]any) error
}

// SnapshotMeta 是快照元信息（采集时的设备环境）。
type SnapshotMeta struct {
	DeviceID   string    `json:"device_id"`
	Model      string    `json:"model"`
	Firmware   string    `json:"firmware"`
	CapturedAt time.Time `json:"captured_at"`
	Source     string    `json:"source"`
}

// Snapshot 是全量快照 JSON 结构（{"meta":{...},"packages":{...}}）。
type Snapshot struct {
	Meta     SnapshotMeta             `json:"meta"`
	Packages map[string]PackageValues `json:"packages"`
}

// Marshal 序列化为紧凑 JSON（encoding/json 对 map key 自动排序，输出稳定）。
func (s *Snapshot) Marshal() ([]byte, error) {
	return json.Marshal(s)
}

// HashSHA256 计算规范化内容指纹：移除全部 .index 后序列化取 SHA-256。
// .index 仅表示 section 在文件中的顺序，不参与内容指纹（设计文档 6.3）。
func (s *Snapshot) HashSHA256() (string, error) {
	normalized := map[string]any{
		"meta":     s.Meta,
		"packages": stripIndex(s.Packages),
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// stripIndex 深拷贝 packages 并移除所有 section 的 .index 元字段。
func stripIndex(pkgs map[string]PackageValues) map[string]PackageValues {
	out := make(map[string]PackageValues, len(pkgs))
	for pkg, values := range pkgs {
		nv := make(PackageValues, len(values))
		for sec, kv := range values {
			nkv := make(SectionValues, len(kv))
			for k, v := range kv {
				if k == ".index" {
					continue
				}
				nkv[k] = v
			}
			nv[sec] = nkv
		}
		out[pkg] = nv
	}
	return out
}

// Collect 从设备采集全量快照：
//  1. uci configs 列出全部配置包；
//  2. 逐包 uci get 读取 values；
//  3. 密钥 option 掩码；
//  4. 组装 Snapshot。
//
// 只读操作，不调用任何写接口。单包失败时记录 partial_errors 并继续（不因一个包失败丢整份快照）。
func Collect(ctx context.Context, ubus UbusCaller, sessionID string, meta SnapshotMeta) (*Snapshot, []error, error) {
	var cfgResp struct {
		Configs []string `json:"configs"`
	}
	if err := ubus.CallInto(ctx, sessionID, "uci", "configs", &cfgResp, map[string]any{}); err != nil {
		return nil, nil, fmt.Errorf("uci configs: %w", err)
	}
	if len(cfgResp.Configs) == 0 {
		return nil, nil, fmt.Errorf("uci configs: empty package list")
	}
	sort.Strings(cfgResp.Configs)

	snap := &Snapshot{
		Meta:     meta,
		Packages: make(map[string]PackageValues, len(cfgResp.Configs)),
	}
	var partial []error
	for _, pkg := range cfgResp.Configs {
		var getResp struct {
			Values PackageValues `json:"values"`
		}
		if err := ubus.CallInto(ctx, sessionID, "uci", "get", &getResp, map[string]any{"config": pkg}); err != nil {
			partial = append(partial, fmt.Errorf("uci get %s: %w", pkg, err))
			continue
		}
		MaskSecrets(getResp.Values)
		snap.Packages[pkg] = getResp.Values
	}
	if len(snap.Packages) == 0 {
		return nil, partial, fmt.Errorf("no package captured (%d errors)", len(partial))
	}
	return snap, partial, nil
}
