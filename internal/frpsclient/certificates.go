package frpsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// ---------- 证书管理（/api/v1/certificates） ----------

// CertificateMeta 是证书槽位元数据（GET /{purpose} 的 meta）。
type CertificateMeta struct {
	Source            string   `json:"source"`
	Version           int      `json:"version"`
	Status            string   `json:"status"`
	LastError         string   `json:"last_error"`
	Comment           string   `json:"comment"`
	HasKey            bool     `json:"has_key"`
	AppliedAt         string   `json:"applied_at"`
	Subject           string   `json:"subject"`
	Issuer            string   `json:"issuer"`
	Serial            string   `json:"serial"`
	KeyType           string   `json:"key_type"`
	KeyBits           int      `json:"key_bits"`
	NotBefore         string   `json:"not_before"`
	NotAfter          string   `json:"not_after"`
	DaysLeft          int      `json:"days_left"`
	SANs              []string `json:"sans"`
	FingerprintSHA256 string   `json:"fingerprint_sha256"`
	SPKISHA256        string   `json:"spki_sha256"`
}

// CertificateItem 是 GET /api/v1/certificates 的列表项（meta 字段平铺在顶层）。
type CertificateItem struct {
	Purpose           string   `json:"purpose"`
	Required          bool     `json:"required"`
	Source            string   `json:"source"`
	Version           int      `json:"version"`
	HasKey            bool     `json:"has_key"`
	Status            string   `json:"status"`
	Warnings          []string `json:"warnings"`
	LastError         string   `json:"last_error"`
	AppliedAt         string   `json:"applied_at"`
	Subject           string   `json:"subject"`
	Issuer            string   `json:"issuer"`
	Serial            string   `json:"serial"`
	KeyType           string   `json:"key_type"`
	KeyBits           int      `json:"key_bits"`
	NotBefore         string   `json:"not_before"`
	NotAfter          string   `json:"not_after"`
	DaysLeft          int      `json:"days_left"`
	SANs              []string `json:"sans"`
	FingerprintSHA256 string   `json:"fingerprint_sha256"`
	SPKISHA256        string   `json:"spki_sha256"`
}

// CertificateDetail 是 GET /api/v1/certificates/{purpose} 的 body。
type CertificateDetail struct {
	Purpose  string          `json:"purpose"`
	CertPEM  string          `json:"cert_pem"`
	ChainPEM string          `json:"chain_pem"`
	KeyPEM   string          `json:"key_pem,omitempty"`
	Meta     CertificateMeta `json:"meta"`
}

// CertificatePutRequest 是 PUT /{purpose} 与 POST /{purpose}/validate 的请求体。
type CertificatePutRequest struct {
	CertPEM       string `json:"cert_pem"`
	ChainPEM      string `json:"chain_pem"`
	KeyPEM        string `json:"key_pem"`
	Comment       string `json:"comment,omitempty"`
	BackupComment string `json:"backup_comment,omitempty"`
	ApplyNow      *bool  `json:"apply_now,omitempty"`
	KeepPrevious  *bool  `json:"keep_previous,omitempty"`
	Force         bool   `json:"force,omitempty"`
}

// CertificateGenerateRequest 是 POST /{purpose}/generate 的请求体。
type CertificateGenerateRequest struct {
	CommonName   string   `json:"common_name,omitempty"`
	SANs         []string `json:"sans,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Days         int      `json:"days,omitempty"`
	KeyType      string   `json:"key_type,omitempty"`
	IsCA         bool     `json:"is_ca,omitempty"`
}

// CertificateRollbackRequest 是 POST /{purpose}/rollback 的请求体。
type CertificateRollbackRequest struct {
	Version       int   `json:"version"`
	BackupComment string `json:"backup_comment,omitempty"`
	ApplyNow      *bool `json:"apply_now,omitempty"`
	Force         bool  `json:"force,omitempty"`
}

// CertificateRestoreRequest 是 POST /backups/{id}/restore 的请求体。
type CertificateRestoreRequest struct {
	BackupComment string `json:"backup_comment,omitempty"`
	ApplyNow      *bool  `json:"apply_now,omitempty"`
	Force         bool   `json:"force,omitempty"`
}

// CertificateBackupItem 是备份列表条目（不含私钥）。
type CertificateBackupItem struct {
	ID                uint   `json:"id"`
	Purpose           string `json:"purpose"`
	Source            string `json:"source"`
	Version           int    `json:"version"`
	Subject           string `json:"subject"`
	NotAfter          string `json:"not_after"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	Trigger           string `json:"trigger"`
	Comment           string `json:"comment"`
	HasKey            bool   `json:"has_key"`
	Protected         bool   `json:"protected"`
	CreatedAt         string `json:"created_at"`
}

// CertificateBackupList 是 GET /backups 的 body。
type CertificateBackupList struct {
	Total    int64                  `json:"total"`
	Page     int                    `json:"page"`
	PageSize int                    `json:"page_size"`
	Items    []CertificateBackupItem `json:"items"`
}

// CertificateVersionItem 是历史版本条目。
type CertificateVersionItem struct {
	Version           int    `json:"version"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	Comment           string `json:"comment"`
	CreatedAt         string `json:"created_at"`
	HasKey            bool   `json:"has_key"`
	Subject           string `json:"subject"`
	NotAfter          string `json:"not_after"`
}

// CertificateVersionList 是 GET /{purpose}/versions 的 body。
type CertificateVersionList struct {
	Total    int64                    `json:"total"`
	Page     int                      `json:"page"`
	PageSize int                      `json:"page_size"`
	Items    []CertificateVersionItem `json:"items"`
}

// CertificateResponse 是证书接口的原始信封。
// 业务失败（如校验不通过、CA 轮换冲突）时 Body 仍可能携带 errors/warnings/
// affected_credentials，故不在此层直接转 APIError，交由调用方（UI 代理）透传给浏览器。
type CertificateResponse struct {
	ErrorCode int             `json:"error_code"`
	Msg       string          `json:"msg"`
	Body      json.RawMessage `json:"body"`
}

// certificateCall 发送任意方法的证书请求并返回原始信封。
func (c *Client) certificateCall(ctx context.Context, method, path string, payload any, query url.Values) (*CertificateResponse, error) {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	data, err := c.send(ctx, method, path, payload)
	if err != nil {
		return nil, err
	}
	var resp CertificateResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode certificate response: %w", err)
	}
	return &resp, nil
}

// certificatePath 构造 /api/v1/certificates/{purpose}[/{action}]。
func certificatePath(purpose, action string) string {
	p := "/api/v1/certificates/" + url.PathEscape(purpose)
	if action != "" {
		p += "/" + action
	}
	return p
}

// ListCertificates 查询三个证书槽位概览。
func (c *Client) ListCertificates(ctx context.Context) ([]CertificateItem, error) {
	data, err := c.get(ctx, "/api/v1/certificates", nil)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var res struct {
		Items []CertificateItem `json:"items"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode certificate list: %w", err)
	}
	return res.Items, nil
}

// GetCertificateDetail 查询槽位详情（includeKey=true 时返回私钥）。
func (c *Client) GetCertificateDetail(ctx context.Context, purpose string, includeKey bool) (*CertificateDetail, error) {
	q := url.Values{}
	if includeKey {
		q.Set("include", "key")
	}
	data, err := c.get(ctx, certificatePath(purpose, ""), q)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var d CertificateDetail
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("decode certificate detail: %w", err)
	}
	return &d, nil
}

// ValidateCertificate 只校验不落库。
func (c *Client) ValidateCertificate(ctx context.Context, purpose string, req *CertificatePutRequest) (*CertificateResponse, error) {
	return c.certificateCall(ctx, http.MethodPost, certificatePath(purpose, "validate"), req, nil)
}

// GenerateCertificate 服务端自签（仅 ca/https，返回内容不落库）。
func (c *Client) GenerateCertificate(ctx context.Context, purpose string, req *CertificateGenerateRequest) (*CertificateResponse, error) {
	return c.certificateCall(ctx, http.MethodPost, certificatePath(purpose, "generate"), req, nil)
}

// PutCertificate 保存证书内容（可选热生效）。
func (c *Client) PutCertificate(ctx context.Context, purpose string, req *CertificatePutRequest) (*CertificateResponse, error) {
	return c.certificateCall(ctx, http.MethodPut, certificatePath(purpose, ""), req, nil)
}

// ApplyCertificate 应用已保存版本（热生效）。
func (c *Client) ApplyCertificate(ctx context.Context, purpose string) (*CertificateResponse, error) {
	return c.certificateCall(ctx, http.MethodPost, certificatePath(purpose, "apply"), nil, nil)
}

// DeleteCertificate 删除数据库证书记录并回退文件/自签。
func (c *Client) DeleteCertificate(ctx context.Context, purpose string, force bool, backupComment string) (*CertificateResponse, error) {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	if backupComment != "" {
		q.Set("backup_comment", backupComment)
	}
	return c.certificateCall(ctx, http.MethodDelete, certificatePath(purpose, ""), nil, q)
}

// ListCertificateVersions 查询历史版本。
func (c *Client) ListCertificateVersions(ctx context.Context, purpose string, page, pageSize int) (*CertificateVersionList, error) {
	q := url.Values{}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	if pageSize > 0 {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	data, err := c.get(ctx, certificatePath(purpose, "versions"), q)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var res CertificateVersionList
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode certificate versions: %w", err)
	}
	return &res, nil
}

// RollbackCertificate 回滚到历史版本。
func (c *Client) RollbackCertificate(ctx context.Context, purpose string, req *CertificateRollbackRequest) (*CertificateResponse, error) {
	return c.certificateCall(ctx, http.MethodPost, certificatePath(purpose, "rollback"), req, nil)
}

// ClearPreviousCA 清除 CA 轮换保留的旧私钥。
func (c *Client) ClearPreviousCA(ctx context.Context, force bool, backupComment string) (*CertificateResponse, error) {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	if backupComment != "" {
		q.Set("backup_comment", backupComment)
	}
	return c.certificateCall(ctx, http.MethodDelete, certificatePath("ca", "previous"), nil, q)
}

// ListCertificateBackups 查询备份列表。
func (c *Client) ListCertificateBackups(ctx context.Context, page, pageSize int) (*CertificateBackupList, error) {
	q := url.Values{}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	if pageSize > 0 {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	data, err := c.get(ctx, "/api/v1/certificates/backups", q)
	if err != nil {
		return nil, err
	}
	body, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	var res CertificateBackupList
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode certificate backups: %w", err)
	}
	return &res, nil
}

// DeleteCertificateBackup 删除指定备份（每槽位最新一条受保护，服务端会拒绝）。
func (c *Client) DeleteCertificateBackup(ctx context.Context, id uint) (*CertificateResponse, error) {
	return c.certificateCall(ctx, http.MethodDelete, "/api/v1/certificates/backups/"+strconv.FormatUint(uint64(id), 10), nil, nil)
}

// RestoreCertificateBackup 恢复指定备份（恢复前自动备份当前材料）。
func (c *Client) RestoreCertificateBackup(ctx context.Context, id uint, req *CertificateRestoreRequest) (*CertificateResponse, error) {
	return c.certificateCall(ctx, http.MethodPost, "/api/v1/certificates/backups/"+strconv.FormatUint(uint64(id), 10)+"/restore", req, nil)
}
