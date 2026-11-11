package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/frpsclient"
)

// ---------- 证书管理（系统设置页「证书管理」TAB） ----------
//
// 数据来自 frps_helper 北向证书管理接口；浏览器只与本服务同域交互，
// 由后端代理到 https://127.0.0.1:7443（管理接口通常不对外暴露）。

// certCard 视图：把北向列表项转成模板可直接展示的字段。
type certCard struct {
	Purpose     string
	Label       string
	Hint        string
	Required    bool
	HasKey      bool
	Version     int
	SourceLabel string
	StatusLabel string
	StatusClass string
	Subject     string
	Issuer      string
	NotBefore   string
	NotAfter    string
	DaysLeft    int
	KeyType     string
	KeyBits     int
	SANs        string
	Fingerprint string
	Warnings    []certWarn
	LastError   string
	CanGenerate bool
}

// certWarn 告警码的中文展示。
type certWarn struct {
	Label string
	Class string
}

// certCards 按固定顺序（ca / server-auth / https）生成卡片视图。
func certCards(items []frpsclient.CertificateItem) []certCard {
	order := map[string]int{"ca": 0, "server-auth": 1, "https": 2}
	sort.SliceStable(items, func(i, j int) bool {
		return order[items[i].Purpose] < order[items[j].Purpose]
	})
	cards := make([]certCard, 0, len(items))
	for _, it := range items {
		card := certCard{
			Purpose:     it.Purpose,
			Label:       certPurposeLabel(it.Purpose),
			Hint:        certPurposeHint(it.Purpose),
			Required:    it.Required,
			HasKey:      it.HasKey,
			Version:     it.Version,
			SourceLabel: certSourceLabel(it.Source),
			StatusLabel: certStatusLabel(it.Status),
			StatusClass: certStatusClass(it.Status),
			Subject:     it.Subject,
			Issuer:      it.Issuer,
			NotBefore:   fmtCertTime(it.NotBefore),
			NotAfter:    fmtCertTime(it.NotAfter),
			DaysLeft:    it.DaysLeft,
			KeyType:     it.KeyType,
			KeyBits:     it.KeyBits,
			SANs:        strings.Join(it.SANs, ", "),
			Fingerprint: it.FingerprintSHA256,
			Warnings:    certWarnViews(it.Warnings),
			LastError:   it.LastError,
			CanGenerate: it.Purpose == "ca" || it.Purpose == "https",
		}
		cards = append(cards, card)
	}
	return cards
}

func certPurposeLabel(p string) string {
	switch p {
	case "ca":
		return "设备密码加密 CA"
	case "server-auth":
		return "双向认证服务器证书"
	case "https":
		return "HTTPS 服务证书"
	default:
		return p
	}
}

func certPurposeHint(p string) string {
	switch p {
	case "ca":
		return "设备默认密码 RSA-OAEP 加解密 + 双向认证签名（RSA ≥ 2048，自签根）"
	case "server-auth":
		return "join 下发证书链 + 双向认证响应签名（需外部 CA 签发）"
	case "https":
		return "6999 设备接口与 7443 管理接口 TLS（支持 RSA / ECDSA）"
	default:
		return ""
	}
}

func certSourceLabel(s string) string {
	switch s {
	case "db":
		return "数据库"
	case "file":
		return "配置文件"
	case "generated":
		return "运行时自签"
	case "none", "":
		return "缺失"
	default:
		return s
	}
}

func certStatusLabel(s string) string {
	switch s {
	case "active":
		return "已生效"
	case "unapplied":
		return "已保存未生效"
	case "apply_failed":
		return "生效失败"
	case "missing", "":
		return "缺失"
	default:
		return s
	}
}

func certStatusClass(s string) string {
	switch s {
	case "active":
		return "success"
	case "unapplied":
		return "warning"
	case "apply_failed":
		return "danger"
	default:
		return "secondary"
	}
}

func certWarnViews(codes []string) []certWarn {
	if len(codes) == 0 {
		return nil
	}
	views := make([]certWarn, 0, len(codes))
	for _, code := range codes {
		switch code {
		case "expiring_soon":
			views = append(views, certWarn{"30 天内到期", "warning"})
		case "self_signed_test_only":
			views = append(views, certWarn{"自签证书，仅建议测试环境使用", "secondary"})
		case "unknown_trust_root":
			views = append(views, certWarn{"证书链存在未知根，客户端可能不信任", "warning"})
		case "san_not_cover_host":
			views = append(views, certWarn{"SAN 未覆盖服务域名/IP", "warning"})
		case "previous_ca_retained":
			views = append(views, certWarn{"已保留旧 CA 私钥用于历史密码解密", "info"})
		case "certificate_missing":
			views = append(views, certWarn{"证书缺失（必需槽位将阻止启动）", "danger"})
		case "cert_expired":
			views = append(views, certWarn{"证书已过期", "danger"})
		default:
			views = append(views, certWarn{code, "secondary"})
		}
	}
	return views
}

// fmtCertTime 把 RFC3339 转成本地展示格式；空值/解析失败返回 "-"。
func fmtCertTime(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02 15:04")
	}
	return s
}

// ---------- 北向代理 ----------

// certForward 原样转发 frps_helper 信封：HTTP 200 + code/message/data。
// 业务失败时 body 仍携带 errors/warnings（前端据此展示）。
func certForward(c *gin.Context, resp *frpsclient.CertificateResponse, err error) {
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	var data any
	if len(resp.Body) > 0 && string(resp.Body) != "null" {
		data = resp.Body
	}
	c.JSON(http.StatusOK, gin.H{"code": resp.ErrorCode, "message": resp.Msg, "data": data})
}

// certPurposeParam 读取并校验路径中的槽位 purpose。
func certPurposeParam(c *gin.Context) (string, bool) {
	p := c.Param("purpose")
	switch p {
	case "ca", "server-auth", "https":
		return p, true
	default:
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "invalid purpose"})
		return "", false
	}
}

// handleCertDetailAPI 槽位详情（回填证书内容；私钥不通过本代理回显）。
func (a *App) handleCertDetailAPI(c *gin.Context) {
	purpose, ok := certPurposeParam(c)
	if !ok {
		return
	}
	d, err := a.Frps.GetCertificateDetail(c.Request.Context(), purpose, false)
	if err != nil {
		var apiErr *frpsclient.APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			// 数据库无记录时槽位仍在用文件/自签材料，属正常状态而非错误。
			c.JSON(http.StatusOK, gin.H{"code": -1005, "message": "数据库暂无记录，当前使用文件/自签材料"})
			return
		}
		a.Log.Error("get certificate detail failed", "purpose", purpose, "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": d})
}

// handleCertValidateAPI 只校验不落库。
func (a *App) handleCertValidateAPI(c *gin.Context) {
	purpose, ok := certPurposeParam(c)
	if !ok {
		return
	}
	var req frpsclient.CertificatePutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "bad_json: " + err.Error()})
		return
	}
	resp, err := a.Frps.ValidateCertificate(c.Request.Context(), purpose, &req)
	if err != nil {
		a.Log.Error("validate certificate failed", "purpose", purpose, "err", err)
	}
	certForward(c, resp, err)
}

// handleCertPutAPI 保存证书内容（默认热生效）。
func (a *App) handleCertPutAPI(c *gin.Context) {
	purpose, ok := certPurposeParam(c)
	if !ok {
		return
	}
	var req frpsclient.CertificatePutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "bad_json: " + err.Error()})
		return
	}
	resp, err := a.Frps.PutCertificate(c.Request.Context(), purpose, &req)
	if err != nil {
		a.Log.Error("put certificate failed", "purpose", purpose, "err", err)
	}
	certForward(c, resp, err)
}

// handleCertDeleteAPI 删除数据库证书记录并回退文件/自签。
func (a *App) handleCertDeleteAPI(c *gin.Context) {
	purpose, ok := certPurposeParam(c)
	if !ok {
		return
	}
	force := c.Query("force") == "1" || c.Query("force") == "true"
	resp, err := a.Frps.DeleteCertificate(c.Request.Context(), purpose, force, c.Query("backup_comment"))
	if err != nil {
		a.Log.Error("delete certificate failed", "purpose", purpose, "err", err)
	}
	certForward(c, resp, err)
}

// handleCertApplyAPI 应用已保存版本。
func (a *App) handleCertApplyAPI(c *gin.Context) {
	purpose, ok := certPurposeParam(c)
	if !ok {
		return
	}
	resp, err := a.Frps.ApplyCertificate(c.Request.Context(), purpose)
	if err != nil {
		a.Log.Error("apply certificate failed", "purpose", purpose, "err", err)
	}
	certForward(c, resp, err)
}

// handleCertGenerateAPI 服务端自签（返回内容供确认后再保存）。
func (a *App) handleCertGenerateAPI(c *gin.Context) {
	purpose, ok := certPurposeParam(c)
	if !ok {
		return
	}
	var req frpsclient.CertificateGenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "bad_json: " + err.Error()})
		return
	}
	resp, err := a.Frps.GenerateCertificate(c.Request.Context(), purpose, &req)
	if err != nil {
		a.Log.Error("generate certificate failed", "purpose", purpose, "err", err)
	}
	certForward(c, resp, err)
}

// handleCertVersionsAPI 历史版本列表。
func (a *App) handleCertVersionsAPI(c *gin.Context) {
	purpose, ok := certPurposeParam(c)
	if !ok {
		return
	}
	page := atoiDefault(c.Query("page"), 1)
	size := atoiDefault(c.Query("page_size"), 20)
	if size > certPageSizeCap {
		size = certPageSizeCap
	}
	list, err := a.Frps.ListCertificateVersions(c.Request.Context(), purpose, page, size)
	if err != nil {
		a.Log.Error("list certificate versions failed", "purpose", purpose, "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": list})
}

// handleCertRollbackAPI 回滚到历史版本。
func (a *App) handleCertRollbackAPI(c *gin.Context) {
	purpose, ok := certPurposeParam(c)
	if !ok {
		return
	}
	var req frpsclient.CertificateRollbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "bad_json: " + err.Error()})
		return
	}
	if req.Version <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "version is required"})
		return
	}
	resp, err := a.Frps.RollbackCertificate(c.Request.Context(), purpose, &req)
	if err != nil {
		a.Log.Error("rollback certificate failed", "purpose", purpose, "err", err)
	}
	certForward(c, resp, err)
}

// handleCertClearPreviousAPI 清除 CA 轮换保留的旧私钥。
func (a *App) handleCertClearPreviousAPI(c *gin.Context) {
	force := c.Query("force") == "1" || c.Query("force") == "true"
	resp, err := a.Frps.ClearPreviousCA(c.Request.Context(), force, c.Query("backup_comment"))
	if err != nil {
		a.Log.Error("clear previous CA failed", "err", err)
	}
	certForward(c, resp, err)
}

// ---------- 备份 ----------

// handleCertBackupsAPI 备份列表（服务端标记每槽位最新一条为受保护）。
func (a *App) handleCertBackupsAPI(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	size := atoiDefault(c.Query("page_size"), 20)
	if size > certPageSizeCap {
		size = certPageSizeCap
	}
	list, err := a.Frps.ListCertificateBackups(c.Request.Context(), page, size)
	if err != nil {
		a.Log.Error("list certificate backups failed", "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": list})
}

// handleCertBackupDeleteAPI 删除备份；最新一条会被服务端拒绝（只能恢复）。
func (a *App) handleCertBackupDeleteAPI(c *gin.Context) {
	id := atoiDefault(c.Param("id"), 0)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "invalid backup id"})
		return
	}
	resp, err := a.Frps.DeleteCertificateBackup(c.Request.Context(), uint(id))
	if err != nil {
		a.Log.Error("delete certificate backup failed", "id", id, "err", err)
	}
	certForward(c, resp, err)
}

// handleCertBackupRestoreAPI 恢复备份（恢复前自动备份当前材料）。
func (a *App) handleCertBackupRestoreAPI(c *gin.Context) {
	id := atoiDefault(c.Param("id"), 0)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "invalid backup id"})
		return
	}
	var req frpsclient.CertificateRestoreRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "message": "bad_json: " + err.Error()})
		return
	}
	resp, err := a.Frps.RestoreCertificateBackup(c.Request.Context(), uint(id), &req)
	if err != nil {
		a.Log.Error("restore certificate backup failed", "id", id, "err", err)
	}
	certForward(c, resp, err)
}

// certPageSizeCap 历史版本分页上限（防止误传超大值）。
const certPageSizeCap = 100
