package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/portal"
	"wrthub-ui/internal/store"
)

// portalPageURL 构造带 flash 的回跳地址。
func portalPageURL(base string, okMsg, errMsg string) string {
	q := url.Values{}
	if okMsg != "" {
		q.Set("ok", okMsg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// portalRedirect 写操作统一回跳（POST → redirect + flash 查询参数）。
func portalRedirect(c *gin.Context, base, okMsg, errMsg string) {
	c.Redirect(http.StatusFound, portalPageURL(base, okMsg, errMsg))
}

// newPortalID 生成 UUIDv4 主键。
func newPortalID() string { return uuid.NewString() }

// injectPortalConfig 在 html 的 </head> 前注入 window.PORTAL_CONFIG（JSON 已做 HTML 转义，防 </script> 逃逸）。
func injectPortalConfig(html []byte, cfg map[string]string) []byte {
	b, err := json.Marshal(cfg)
	if err != nil {
		return html
	}
	snip := []byte("<script>window.PORTAL_CONFIG=" + string(b) + ";</script>")
	if i := bytes.Index(bytes.ToLower(html), []byte("</head>")); i >= 0 {
		out := make([]byte, 0, len(html)+len(snip))
		out = append(out, html[:i]...)
		out = append(out, snip...)
		out = append(out, html[i:]...)
		return out
	}
	return append(snip, html...)
}

// ---------------- 认证模板 ----------------

// portalTemplatesPage 认证模板列表页视图。
type portalTemplatesPage struct {
	pageBase
	Templates  []store.PortalTemplate
	Total      int
	Page       int
	PageSize   int
	TotalPages int
	Keyword    string
	Status     string
	Pages      []pageLink
	FlashOK    string
	FlashErr   string
}

// handlePortalTemplates 渲染认证模板列表页。
func (a *App) handlePortalTemplates(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	pageSize := atoiDefault(c.Query("page_size"), 15)
	keyword := c.Query("keyword")
	status := c.Query("status")

	query := a.DB.Model(&store.PortalTemplate{})
	if keyword != "" {
		query = query.Where("name LIKE ? OR description LIKE ?", "%"+keyword+"%", "%"+keyword+"%")
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		a.Log.Error("count portal templates failed", "err", err)
	}
	var items []store.PortalTemplate
	if err := query.Order("created_at DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error; err != nil {
		a.Log.Error("list portal templates failed", "err", err)
	}
	totalPages := int(total+int64(pageSize)-1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	u, _ := auth.CurrentUser(c)
	f := deviceFilters{Keyword: keyword, Status: status}
	pg := portalTemplatesPage{
		pageBase:   pageBase{ActiveMenu: "portal_templates", CurrentUser: u, Config: a.Cfg},
		Templates:  items,
		Total:      int(total),
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages,
		Keyword:    keyword,
		Status:     status,
		Pages:      buildPageLinks(page, totalPages, f),
		FlashOK:    c.Query("ok"),
		FlashErr:   c.Query("err"),
	}
	if rerr := a.Render.Render(c.Writer, "pages/portal/templates", pg); rerr != nil {
		a.Log.Error("render portal templates failed", "err", rerr)
	}
}

// handlePortalTemplateUpload 上传模板 zip（POST /portal/templates，multipart）。
func (a *App) handlePortalTemplateUpload(c *gin.Context) {
	back := "/portal/templates"
	errMsg := func(msg string) { portalRedirect(c, back, "", msg) }

	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		errMsg("上传失败：模板名称不能为空")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, portal.MaxZipBytes+(1<<20))
	fh, err := c.FormFile("file")
	if err != nil {
		errMsg("上传失败：请选择模板 zip 包（≤10MB）")
		return
	}
	f, err := fh.Open()
	if err != nil {
		errMsg("上传失败：读取文件失败")
		return
	}
	defer f.Close()

	// 大小校验 + 路径/扩展名校验
	fileCount, totalSize, err := portal.ValidateZipReader(f, fh.Size)
	if err != nil {
		a.Log.Warn("portal template zip invalid", "err", err)
		errMsg("上传失败：" + err.Error())
		return
	}

	// 名称唯一预检（uniqueIndex 兜底）
	var cnt int64
	if err := a.DB.Model(&store.PortalTemplate{}).Where("name = ?", name).Count(&cnt).Error; err != nil || cnt > 0 {
		errMsg("上传失败：同名模板已存在")
		return
	}

	id := newPortalID()
	code := strings.ReplaceAll(id, "-", "")[:20]
	entryPage := c.PostForm("entry_page")
	if entryPage == "" {
		entryPage = "index.html"
	}
	u, _ := auth.CurrentUser(c)
	createdBy := ""
	if u != nil {
		createdBy = u.Username
	}

	// 先落盘再入库；入库失败回滚目录
	if err := a.PortalStore.Extract(code, f, fh.Size); err != nil {
		_ = a.PortalStore.Remove(code)
		a.Log.Error("portal template extract failed", "err", err)
		errMsg("上传失败：" + err.Error())
		return
	}
	if !a.PortalStore.HasEntry(code, entryPage) {
		_ = a.PortalStore.Remove(code)
		errMsg("上传失败：入口页 " + entryPage + " 不在模板包内")
		return
	}
	t := &store.PortalTemplate{
		ID:          id,
		Code:        code,
		Name:        name,
		Description: c.PostForm("description"),
		EntryPage:   entryPage,
		Status:      "enabled",
		FileCount:   fileCount,
		TotalSize:   totalSize,
		HasSchema:   a.PortalStore.HasSchemaFile(code),
		CreatedBy:   createdBy,
	}
	if err := a.DB.Create(t).Error; err != nil {
		_ = a.PortalStore.Remove(code)
		a.Log.Error("create portal template failed", "err", err)
		errMsg("上传失败：保存模板失败")
		return
	}
	portalRedirect(c, back, "模板「"+name+"」已上传", "")
}

// loadPortalTemplate 按主键加载模板。
func (a *App) loadPortalTemplate(id string) (*store.PortalTemplate, error) {
	var t store.PortalTemplate
	if err := a.DB.First(&t, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// handlePortalTemplateToggle 启用/停用模板（POST /portal/templates/:id/toggle）。
func (a *App) handlePortalTemplateToggle(c *gin.Context) {
	back := "/portal/templates"
	t, err := a.loadPortalTemplate(c.Param("id"))
	if err != nil {
		portalRedirect(c, back, "", "模板不存在")
		return
	}
	if t.Status == "enabled" {
		t.Status = "disabled"
	} else {
		t.Status = "enabled"
	}
	t.Version++
	if err := a.DB.Save(t).Error; err != nil {
		a.Log.Error("toggle portal template failed", "err", err)
		portalRedirect(c, back, "", "操作失败："+err.Error())
		return
	}
	portalRedirect(c, back, "模板「"+t.Name+"」已"+map[string]string{"enabled": "启用", "disabled": "停用"}[t.Status], "")
}

// handlePortalTemplateDelete 删除模板（POST /portal/templates/:id/delete）。
// 被策略引用时拒绝删除（与 tentant-mng 语义一致）。
func (a *App) handlePortalTemplateDelete(c *gin.Context) {
	back := "/portal/templates"
	t, err := a.loadPortalTemplate(c.Param("id"))
	if err != nil {
		portalRedirect(c, back, "", "模板不存在")
		return
	}
	if t.IsSystem {
		portalRedirect(c, back, "", "系统默认模板不可删除")
		return
	}
	var cnt int64
	if err := a.DB.Model(&store.PortalPolicy{}).Where("template_id = ?", t.ID).Count(&cnt).Error; err != nil {
		portalRedirect(c, back, "", "检查模板引用失败")
		return
	}
	if cnt > 0 {
		portalRedirect(c, back, "", "模板被 "+strconv.FormatInt(cnt, 10)+" 个策略引用，无法删除")
		return
	}
	if err := a.DB.Delete(t).Error; err != nil {
		portalRedirect(c, back, "", "删除失败："+err.Error())
		return
	}
	if err := a.PortalStore.Remove(t.Code); err != nil {
		a.Log.Warn("remove portal template dir failed", "code", t.Code, "err", err)
	}
	portalRedirect(c, back, "模板「"+t.Name+"」已删除", "")
}

// handlePortalTemplateFork 复制模板（POST /portal/templates/:id/fork，form: name）。
func (a *App) handlePortalTemplateFork(c *gin.Context) {
	back := "/portal/templates"
	src, err := a.loadPortalTemplate(c.Param("id"))
	if err != nil {
		portalRedirect(c, back, "", "模板不存在")
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		name = src.Name + " 副本"
	}
	var cnt int64
	if err := a.DB.Model(&store.PortalTemplate{}).Where("name = ?", name).Count(&cnt).Error; err != nil || cnt > 0 {
		portalRedirect(c, back, "", "复制失败：同名模板已存在")
		return
	}
	id := newPortalID()
	code := strings.ReplaceAll(id, "-", "")[:20]
	if err := a.PortalStore.Fork(src.Code, code); err != nil {
		_ = a.PortalStore.Remove(code)
		a.Log.Error("fork portal template dir failed", "err", err)
		portalRedirect(c, back, "", "复制失败："+err.Error())
		return
	}
	u, _ := auth.CurrentUser(c)
	createdBy := ""
	if u != nil {
		createdBy = u.Username
	}
	dst := *src
	dst.ID = id
	dst.Code = code
	dst.Name = name
	dst.Status = "enabled"
	dst.CreatedBy = createdBy
	dst.IsSystem = false // 复制出的模板属于用户模板，可删除
	dst.Version = 1
	dst.CreatedAt = time.Time{}
	dst.UpdatedAt = time.Time{}
	if err := a.DB.Create(&dst).Error; err != nil {
		_ = a.PortalStore.Remove(code)
		a.Log.Error("fork portal template db failed", "err", err)
		portalRedirect(c, back, "", "复制失败：保存模板失败")
		return
	}
	portalRedirect(c, back, "已复制为「"+name+"」", "")
}

// handlePortalTemplateFiles 模板在线编辑页（GET /portal/templates/:id/files?path=xxx）。
// 预览与编辑合一：默认打开入口页，左侧预览、右侧代码编辑，顶部 tab 切换文件。
func (a *App) handlePortalTemplateFiles(c *gin.Context) {
	t, err := a.loadPortalTemplate(c.Param("id"))
	if err != nil {
		portalRedirect(c, "/portal/templates", "", "模板不存在")
		return
	}
	files, ferr := a.PortalStore.Files(t.Code)
	if ferr != nil {
		a.Log.Error("scan portal template files failed", "code", t.Code, "err", ferr)
	}

	p := c.Query("path")
	if p == "" {
		p = t.EntryPage
	}
	if _, verr := portal.CleanRelPath(p); verr != nil {
		p = t.EntryPage
	}
	content := ""
	editable := portal.IsTextExt(p)
	oversize := false
	readErr := ""
	if editable {
		data, rerr := a.PortalStore.ReadFile(t.Code, p)
		if rerr != nil {
			readErr = "读取文件失败：" + rerr.Error()
		} else if len(data) > portal.MaxEditBytes {
			oversize = true
		} else {
			content = string(data)
		}
	}
	u, _ := auth.CurrentUser(c)
	flashErr := c.Query("err")
	if flashErr == "" {
		flashErr = readErr
	}
	pg := portalEditPage{
		pageBase: pageBase{ActiveMenu: "portal_templates", CurrentUser: u, Config: a.Cfg},
		Tpl:      *t,
		Files:    files,
		Path:     p,
		Content:  content,
		Editable: editable,
		Oversize: oversize,
		NowUnix:  time.Now().Unix(),
		FlashOK:  c.Query("ok"),
		FlashErr: flashErr,
		Tab:      c.Query("tab"),
	}
	pg.Tabs = portalEditorTabs(files, t.EntryPage)
	buildVisualEditor(&pg, a.PortalStore, t)
	if rerr := a.Render.Render(c.Writer, "pages/portal/edit", pg); rerr != nil {
		a.Log.Error("render portal edit failed", "err", rerr)
	}
}

// portalEditorTabs 生成文件 tab 顺序：入口页优先，其余按路径排序（非文本文件排在最后）。
// schema.json 决定可视化编辑的页面/字段，需支持在线修（schema 非法时页面提示源码编辑修复），
// 故排在文本文件末尾、非文本文件之前。
func portalEditorTabs(files []portal.TemplateFile, entry string) []portal.TemplateFile {
	tabs := make([]portal.TemplateFile, 0, len(files))
	for _, f := range files {
		if f.Path == entry {
			tabs = append(tabs, f)
			break
		}
	}
	for _, f := range files {
		if f.Path != entry && f.Path != portal.SchemaFileName && f.Text {
			tabs = append(tabs, f)
		}
	}
	for _, f := range files {
		if f.Path == portal.SchemaFileName {
			tabs = append(tabs, f)
			break
		}
	}
	for _, f := range files {
		if f.Path != entry && f.Path != portal.SchemaFileName && !f.Text {
			tabs = append(tabs, f)
		}
	}
	return tabs
}

// handlePortalTemplatePreview 模板文件预览（GET /portal/templates/:id/preview/*filepath）。
// 静态服务模板包内文件（登录态保护，portal:read）。
func (a *App) handlePortalTemplatePreview(c *gin.Context) {
	t, err := a.loadPortalTemplate(c.Param("id"))
	if err != nil {
		c.String(http.StatusNotFound, "模板不存在")
		return
	}
	rel := strings.TrimPrefix(c.Param("filepath"), "/")
	path, perr := a.PortalStore.Resolve(t.Code, rel)
	if perr != nil {
		c.String(http.StatusNotFound, "模板文件不存在")
		return
	}
	// html 预览注入已保存的可视化配置（window.PORTAL_CONFIG）；
	// 模板 JS 约定：URL query 存在时优先（编辑中预览），否则读 PORTAL_CONFIG。
	ext := strings.ToLower(pathpkg.Ext(rel))
	if (ext == ".html" || ext == ".htm") && t.ConfigJSON != "" {
		if cfg := t.Config(); len(cfg) > 0 {
			if data, rerr := os.ReadFile(path); rerr == nil {
				c.Header("Cache-Control", "no-cache")
				c.Header("Content-Type", portal.ContentType(rel))
				c.Data(http.StatusOK, portal.ContentType(rel), injectPortalConfig(data, cfg))
				return
			}
		}
	}
	// 用 ServeContent 而非 c.File：ServeFile 对以 /index.html 结尾的 URL 会 301 到 ./，
	// 会干扰入口页预览；ServeContent 仅按内容服务（Content-Type 按扩展名显式给出）。
	f, err := os.Open(path)
	if err != nil {
		c.String(http.StatusInternalServerError, "读取模板文件失败")
		return
	}
	defer f.Close()
	st, serr := f.Stat()
	if serr != nil {
		c.String(http.StatusInternalServerError, "读取模板文件失败")
		return
	}
	c.Header("Cache-Control", "private, max-age=60")
	c.Header("Content-Type", portal.ContentType(rel))
	http.ServeContent(c.Writer, c.Request, "", st.ModTime(), f)
}

// ---------------- 模板文件在线编辑 ----------------

// editRedirect 统一回编辑页（可携带当前文件路径、目标 tab 与 flash）。
// tab 取值："visual"（可视化编辑）或 ""（源码编辑，默认）。
func editRedirect(c *gin.Context, id, path, tab, okMsg, errMsg string) {
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	if tab != "" {
		q.Set("tab", tab)
	}
	if okMsg != "" {
		q.Set("ok", okMsg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	c.Redirect(http.StatusFound, "/portal/templates/"+id+"/files?"+q.Encode())
}

// portalEditField 可视化编辑表单项（schema 字段 + 当前生效值）。
type portalEditField struct {
	Key       string
	Label     string
	Type      string // text / color / image
	Value     string // 已保存配置值（空=使用默认）
	Default   string
	MaxLength int
	Help      string
}

// portalEditTab 可视化编辑的一个页面分组。
type portalEditTab struct {
	ID     string
	Name   string
	File   string
	Fields []portalEditField
}

// portalEditPage 模板在线编辑页视图。
type portalEditPage struct {
	pageBase
	Tpl      store.PortalTemplate
	Files    []portal.TemplateFile
	Tabs     []portal.TemplateFile // 文件 tab 顺序（入口页优先、非文本排后）
	Path     string                // 当前编辑文件（slash 相对路径）
	Content  string
	Editable bool // 文本类可编辑
	Oversize bool // 超过单文件编辑上限，只读提示
	NowUnix  int64
	FlashOK  string
	FlashErr string
	// 可视化编辑（模板包含合法 schema.json 时可用）
	Visual    bool
	SchemaErr string
	Pages     []portalEditTab
	Images    []string // 包内图片相对路径（image 字段选项）
	Tab       string   // 当前激活 tab：visual / source
}

// handlePortalTemplateEdit 旧链接兼容：统一跳到 /files 在线编辑页。
func (a *App) handlePortalTemplateEdit(c *gin.Context) {
	editRedirect(c, c.Param("id"), c.Query("path"), c.Query("tab"), c.Query("ok"), c.Query("err"))
}

// buildVisualEditor 填充可视化编辑数据：解析 schema.json、合并已保存配置、收集图片选项。
// schema 缺失/非法不阻断源码编辑，仅在页面提示。
func buildVisualEditor(pg *portalEditPage, ps *portal.Store, t *store.PortalTemplate) {
	schema, err := ps.ReadSchema(t.Code)
	if err != nil {
		pg.SchemaErr = "schema.json 无效：" + err.Error()
		return
	}
	if schema == nil {
		return
	}
	cfg := t.Config()
	pg.Visual = true
	pg.Pages = make([]portalEditTab, 0, len(schema.Pages))
	for _, sp := range schema.Pages {
		tab := portalEditTab{ID: sp.ID, Name: sp.Name, File: sp.File}
		for _, sf := range sp.Fields {
			tab.Fields = append(tab.Fields, portalEditField{
				Key:       sf.Key,
				Label:     sf.Label,
				Type:      sf.Type,
				Value:     cfg[sf.Key],
				Default:   sf.Default,
				MaxLength: sf.MaxLength,
				Help:      sf.Help,
			})
		}
		pg.Pages = append(pg.Pages, tab)
	}
	for _, f := range pg.Files {
		if portal.IsImageExt(f.Path) {
			pg.Images = append(pg.Images, f.Path)
		}
	}
	// 默认 tab：有可视化编辑时优先 visual
	if pg.Tab == "" {
		pg.Tab = "visual"
	}
}

// handlePortalTemplateFileSave 保存编辑内容（POST /portal/templates/:id/files/save，form: path, content）。
func (a *App) handlePortalTemplateFileSave(c *gin.Context) {
	id := c.Param("id")
	t, err := a.loadPortalTemplate(id)
	if err != nil {
		portalRedirect(c, "/portal/templates", "", "模板不存在")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, portal.MaxEditBytes+(1<<20))
	p := c.PostForm("path")
	content := c.PostForm("content")
	clean, cerr := portal.CleanRelPath(p)
	if cerr != nil {
		editRedirect(c, id, "", "source", "", "非法的文件路径")
		return
	}
	if !portal.IsTextExt(clean) {
		editRedirect(c, id, clean, "source", "", "该文件类型不支持在线编辑")
		return
	}
	if int64(len(content)) > portal.MaxEditBytes {
		editRedirect(c, id, clean, "source", "", "文件内容超过 512KB 上限")
		return
	}
	if err := a.PortalStore.WriteFile(t.Code, clean, []byte(content)); err != nil {
		a.Log.Error("save portal template file failed", "code", t.Code, "path", clean, "err", err)
		editRedirect(c, id, clean, "source", "", "保存失败："+err.Error())
		return
	}
	a.refreshPortalTemplateMeta(t)
	editRedirect(c, id, clean, "source", "已保存 "+clean, "")
}

// handlePortalTemplateFileCreate 新建文件（POST /portal/templates/:id/files/create，form: path）。
func (a *App) handlePortalTemplateFileCreate(c *gin.Context) {
	id := c.Param("id")
	t, err := a.loadPortalTemplate(id)
	if err != nil {
		portalRedirect(c, "/portal/templates", "", "模板不存在")
		return
	}
	p := c.PostForm("path")
	clean, cerr := portal.CleanRelPath(p)
	if cerr != nil {
		editRedirect(c, id, "", "source", "", "非法的文件路径")
		return
	}
	if !portal.IsTextExt(clean) {
		editRedirect(c, id, "", "source", "", "仅支持新建文本类文件（html/htm/css/js/mjs/json/xml/txt）")
		return
	}
	if _, serr := a.PortalStore.Resolve(t.Code, clean); serr == nil {
		editRedirect(c, id, clean, "source", "", "文件已存在")
		return
	}
	files, _ := a.PortalStore.Files(t.Code)
	if len(files) >= portal.MaxFiles {
		editRedirect(c, id, "", "source", "", "模板文件数已达 200 上限")
		return
	}
	// html 文件给最小骨架，其余为空文件
	content := ""
	base := strings.ToLower(pathpkg.Base(clean))
	if strings.HasSuffix(base, ".html") || strings.HasSuffix(base, ".htm") {
		content = "<!DOCTYPE html>\n<html lang=\"zh-CN\">\n<head>\n  <meta charset=\"utf-8\">\n  <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n  <title>新页面</title>\n</head>\n<body>\n</body>\n</html>\n"
	}
	if err := a.PortalStore.WriteFile(t.Code, clean, []byte(content)); err != nil {
		a.Log.Error("create portal template file failed", "code", t.Code, "path", clean, "err", err)
		editRedirect(c, id, "", "source", "", "新建失败："+err.Error())
		return
	}
	a.refreshPortalTemplateMeta(t)
	editRedirect(c, id, clean, "source", "已新建 "+clean, "")
}

// handlePortalTemplateFileDelete 删除文件（POST /portal/templates/:id/files/delete，form: path）。
func (a *App) handlePortalTemplateFileDelete(c *gin.Context) {
	id := c.Param("id")
	t, err := a.loadPortalTemplate(id)
	if err != nil {
		portalRedirect(c, "/portal/templates", "", "模板不存在")
		return
	}
	p := c.PostForm("path")
	clean, cerr := portal.CleanRelPath(p)
	if cerr != nil {
		editRedirect(c, id, "", "source", "", "非法的文件路径")
		return
	}
	if clean == t.EntryPage {
		editRedirect(c, id, clean, "source", "", "入口页不可删除；如需更换请先修改模板配置")
		return
	}
	if err := a.PortalStore.DeleteFile(t.Code, clean); err != nil {
		a.Log.Error("delete portal template file failed", "code", t.Code, "path", clean, "err", err)
		editRedirect(c, id, clean, "source", "", "删除失败："+err.Error())
		return
	}
	a.refreshPortalTemplateMeta(t)
	editRedirect(c, id, t.EntryPage, "source", "已删除 "+clean, "")
}

// handlePortalTemplateConfigSave 保存可视化编辑配置（POST /portal/templates/:id/config）。
// 表单字段名即 schema.json 声明的 key；仅接受已声明字段，空值=恢复模板内置默认。
func (a *App) handlePortalTemplateConfigSave(c *gin.Context) {
	id := c.Param("id")
	t, err := a.loadPortalTemplate(id)
	if err != nil {
		portalRedirect(c, "/portal/templates", "", "模板不存在")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	schema, serr := a.PortalStore.ReadSchema(t.Code)
	if serr != nil {
		editRedirect(c, id, "", "visual", "", "schema.json 无效："+serr.Error())
		return
	}
	if schema == nil {
		editRedirect(c, id, "", "", "", "该模板未提供 schema.json，不支持可视化编辑")
		return
	}
	raw := make(map[string]string, 32)
	for _, p := range schema.Pages {
		for _, f := range p.Fields {
			raw[f.Key] = c.PostForm(f.Key)
		}
	}
	// image 字段值须为包内存在的图片
	files, _ := a.PortalStore.Files(t.Code)
	imgs := make(map[string]bool, len(files))
	for _, f := range files {
		if portal.IsImageExt(f.Path) {
			imgs[f.Path] = true
		}
	}
	vals, verr := schema.Validate(raw, func(p string) bool { return imgs[p] })
	if verr != nil {
		editRedirect(c, id, "", "visual", "", verr.Error())
		return
	}
	t.SetConfig(vals)
	t.Version++
	if err := a.DB.Save(t).Error; err != nil {
		a.Log.Error("save portal template config failed", "code", t.Code, "err", err)
		editRedirect(c, id, "", "visual", "", "保存失败："+err.Error())
		return
	}
	editRedirect(c, id, "", "visual", "配置已保存", "")
}

// refreshPortalTemplateMeta 文件变更后重算元数据并递增版本。
func (a *App) refreshPortalTemplateMeta(t *store.PortalTemplate) {
	if files, err := a.PortalStore.Files(t.Code); err == nil {
		var total int64
		for _, f := range files {
			total += f.Size
		}
		t.FileCount = len(files)
		t.TotalSize = total
		t.HasSchema = a.PortalStore.HasSchemaFile(t.Code)
	}
	t.Version++
	if err := a.DB.Save(t).Error; err != nil {
		a.Log.Error("refresh portal template meta failed", "code", t.Code, "err", err)
	}
}

// ---------------- 认证策略 ----------------

// weekdayOption 是策略表单"生效日"复选框项。
type weekdayOption struct {
	Val   string
	Label string
}

// weekdayOptions 周一(1) ~ 周日(7)。
var weekdayOptions = []weekdayOption{
	{"1", "周一"}, {"2", "周二"}, {"3", "周三"}, {"4", "周四"},
	{"5", "周五"}, {"6", "周六"}, {"7", "周日"},
}

// portalPoliciesPage 认证策略列表页视图。
type portalPoliciesPage struct {
	pageBase
	Policies      []store.PortalPolicy
	Templates     []store.PortalTemplate
	WeekdayLabels []weekdayOption
	TplNames      map[string]string
	Total         int
	Page          int
	PageSize      int
	TotalPages    int
	Keyword       string
	Status        string
	Pages         []pageLink
	FlashOK       string
	FlashErr      string
}

// handlePortalPolicies 渲染认证策略列表页。
func (a *App) handlePortalPolicies(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	pageSize := atoiDefault(c.Query("page_size"), 15)
	keyword := c.Query("keyword")
	status := c.Query("status")

	query := a.DB.Model(&store.PortalPolicy{})
	if keyword != "" {
		query = query.Where("name LIKE ? OR description LIKE ?", "%"+keyword+"%", "%"+keyword+"%")
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		a.Log.Error("count portal policies failed", "err", err)
	}
	var items []store.PortalPolicy
	if err := query.Order("priority ASC, created_at DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error; err != nil {
		a.Log.Error("list portal policies failed", "err", err)
	}

	// 模板下拉与名称映射（一次取全量，单机管理界面数量有限）
	var templates []store.PortalTemplate
	if err := a.DB.Order("name ASC").Find(&templates).Error; err != nil {
		a.Log.Error("list portal templates for policy failed", "err", err)
	}
	tplNames := make(map[string]string, len(templates))
	for _, t := range templates {
		tplNames[t.ID] = t.Name
	}

	totalPages := int(total+int64(pageSize)-1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	u, _ := auth.CurrentUser(c)
	f := deviceFilters{Keyword: keyword, Status: status}
	pg := portalPoliciesPage{
		pageBase:      pageBase{ActiveMenu: "portal_policies", CurrentUser: u, Config: a.Cfg},
		Policies:      items,
		Templates:     templates,
		WeekdayLabels: weekdayOptions,
		TplNames:      tplNames,
		Total:         int(total),
		Page:          page,
		PageSize:      pageSize,
		TotalPages:    totalPages,
		Keyword:       keyword,
		Status:        status,
		Pages:         buildPageLinks(page, totalPages, f),
		FlashOK:       c.Query("ok"),
		FlashErr:      c.Query("err"),
	}
	if rerr := a.Render.Render(c.Writer, "pages/portal/policies", pg); rerr != nil {
		a.Log.Error("render portal policies failed", "err", rerr)
	}
}

// policyFormPtr 取表单可选字符串字段：空 → nil（不限制）。
func policyFormPtr(c *gin.Context, key string) *string {
	v := strings.TrimSpace(c.PostForm(key))
	if v == "" {
		return nil
	}
	return &v
}

// handlePortalPolicySave 新建/编辑策略（POST /portal/policies；id 空 = 新建）。
func (a *App) handlePortalPolicySave(c *gin.Context) {
	back := "/portal/policies"
	errMsg := func(msg string) { portalRedirect(c, back, "", msg) }

	name := strings.TrimSpace(c.PostForm("name"))
	templateID := strings.TrimSpace(c.PostForm("template_id"))
	if name == "" {
		errMsg("保存失败：策略名称不能为空")
		return
	}
	if templateID == "" {
		errMsg("保存失败：请选择认证模板")
		return
	}
	if _, err := a.loadPortalTemplate(templateID); err != nil {
		errMsg("保存失败：认证模板不存在")
		return
	}

	// 生效日复选框 → "1,2,7"（升序）
	wd := c.PostFormArray("weekdays")
	sort.Strings(wd)
	weekdays := strings.Join(wd, ",")
	priority := atoiDefault(c.PostForm("priority"), 100)
	status := c.PostForm("status")
	if status != "enabled" && status != "disabled" {
		status = "enabled"
	}
	isDefault := c.PostForm("is_default") == "1"

	id := strings.TrimSpace(c.PostForm("id"))
	if id == "" {
		// 新建：名称唯一预检
		var cnt int64
		if err := a.DB.Model(&store.PortalPolicy{}).Where("name = ?", name).Count(&cnt).Error; err != nil || cnt > 0 {
			errMsg("保存失败：同名策略已存在")
			return
		}
		p := &store.PortalPolicy{
			ID:            newPortalID(),
			Name:          name,
			Description:   c.PostForm("description"),
			TemplateID:    templateID,
			MatchSSID:     policyFormPtr(c, "match_ssid"),
			MatchACIP:     policyFormPtr(c, "match_ac_ip"),
			MatchACName:   policyFormPtr(c, "match_ac_name"),
			MatchAPID:     policyFormPtr(c, "match_apid"),
			MatchIPPrefix: policyFormPtr(c, "match_ip_prefix"),
			TimeStart:     policyFormPtr(c, "time_start"),
			TimeEnd:       policyFormPtr(c, "time_end"),
			Weekdays:      nilIfEmpty(weekdays),
			Priority:      priority,
			IsDefault:     isDefault,
			Status:        status,
		}
		if err := a.DB.Create(p).Error; err != nil {
			a.Log.Error("create portal policy failed", "err", err)
			errMsg("保存失败：" + err.Error())
			return
		}
		portalRedirect(c, back, "策略「"+name+"」已创建", "")
		return
	}

	// 编辑
	var p store.PortalPolicy
	if err := a.DB.First(&p, "id = ?", id).Error; err != nil {
		errMsg("策略不存在")
		return
	}
	var nameCnt int64
	if err := a.DB.Model(&store.PortalPolicy{}).Where("name = ? AND id != ?", name, id).Count(&nameCnt).Error; err != nil || nameCnt > 0 {
		errMsg("保存失败：同名策略已存在")
		return
	}
	p.Name = name
	p.Description = c.PostForm("description")
	p.TemplateID = templateID
	p.MatchSSID = policyFormPtr(c, "match_ssid")
	p.MatchACIP = policyFormPtr(c, "match_ac_ip")
	p.MatchACName = policyFormPtr(c, "match_ac_name")
	p.MatchAPID = policyFormPtr(c, "match_apid")
	p.MatchIPPrefix = policyFormPtr(c, "match_ip_prefix")
	p.TimeStart = policyFormPtr(c, "time_start")
	p.TimeEnd = policyFormPtr(c, "time_end")
	p.Weekdays = nilIfEmpty(weekdays)
	p.Priority = priority
	p.IsDefault = isDefault
	p.Status = status
	p.Version++
	if err := a.DB.Save(&p).Error; err != nil {
		a.Log.Error("update portal policy failed", "err", err)
		errMsg("保存失败：" + err.Error())
		return
	}
	portalRedirect(c, back, "策略「"+name+"」已保存", "")
}

// nilIfEmpty 空串转 nil 指针。
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// handlePortalPolicyToggle 启用/停用策略（POST /portal/policies/:id/toggle）。
func (a *App) handlePortalPolicyToggle(c *gin.Context) {
	back := "/portal/policies"
	var p store.PortalPolicy
	if err := a.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		portalRedirect(c, back, "", "策略不存在")
		return
	}
	if p.Status == "enabled" {
		p.Status = "disabled"
	} else {
		p.Status = "enabled"
	}
	p.Version++
	if err := a.DB.Save(&p).Error; err != nil {
		portalRedirect(c, back, "", "操作失败："+err.Error())
		return
	}
	portalRedirect(c, back, "策略「"+p.Name+"」已"+map[string]string{"enabled": "启用", "disabled": "停用"}[p.Status], "")
}

// handlePortalPolicyDelete 删除策略（POST /portal/policies/:id/delete）。
func (a *App) handlePortalPolicyDelete(c *gin.Context) {
	back := "/portal/policies"
	var p store.PortalPolicy
	if err := a.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		portalRedirect(c, back, "", "策略不存在")
		return
	}
	if err := a.DB.Delete(&p).Error; err != nil {
		portalRedirect(c, back, "", "删除失败："+err.Error())
		return
	}
	portalRedirect(c, back, "策略「"+p.Name+"」已删除", "")
}

// ---------------- 认证日志 ----------------

// portalAuthLogsPage 认证日志列表页视图。
type portalAuthLogsPage struct {
	pageBase
	Logs       []store.PortalAuthLog
	Total      int
	Page       int
	PageSize   int
	TotalPages int
	Username   string
	Mac        string
	Status     string
	TimeFrom   string
	TimeTo     string
	Pages      []pageLink
}

// parsePortalTime 解析 datetime-local 值（本地时区）；失败返回零值与 false。
func parsePortalTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02T15:04", s, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// handlePortalAuthLogs 渲染认证日志列表页（只读；默认查最近 24 小时）。
func (a *App) handlePortalAuthLogs(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	pageSize := atoiDefault(c.Query("page_size"), 20)
	username := strings.TrimSpace(c.Query("username"))
	mac := strings.TrimSpace(c.Query("mac"))
	status := c.Query("status")
	timeFrom := c.Query("time_from")
	timeTo := c.Query("time_to")

	query := a.DB.Model(&store.PortalAuthLog{})
	if username != "" {
		query = query.Where("username LIKE ?", "%"+username+"%")
	}
	if mac != "" {
		query = query.Where("mac LIKE ?", "%"+mac+"%")
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	// 时间范围：显式指定优先；否则默认最近 24h（防全表扫描）
	from, okFrom := parsePortalTime(timeFrom)
	to, okTo := parsePortalTime(timeTo)
	if okFrom {
		query = query.Where("request_time >= ?", from)
	}
	if okTo {
		query = query.Where("request_time <= ?", to)
	}
	if !okFrom && !okTo {
		query = query.Where("request_time >= ?", time.Now().Add(-24*time.Hour))
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		a.Log.Error("count portal auth logs failed", "err", err)
	}
	var items []store.PortalAuthLog
	if err := query.Order("request_time DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error; err != nil {
		a.Log.Error("list portal auth logs failed", "err", err)
	}
	totalPages := int(total+int64(pageSize)-1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	u, _ := auth.CurrentUser(c)
	pg := portalAuthLogsPage{
		pageBase:   pageBase{ActiveMenu: "portal_auth_logs", CurrentUser: u, Config: a.Cfg},
		Logs:       items,
		Total:      int(total),
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages,
		Username:   username,
		Mac:        mac,
		Status:     status,
		TimeFrom:   timeFrom,
		TimeTo:     timeTo,
		Pages:      portalAuthLogPageLinks(page, totalPages, username, mac, status, timeFrom, timeTo),
	}
	if rerr := a.Render.Render(c.Writer, "pages/portal/auth_logs", pg); rerr != nil {
		a.Log.Error("render portal auth logs failed", "err", rerr)
	}
}

// portalAuthLogPageURL 构造认证日志分页链接（保留筛选条件）。
func portalAuthLogPageURL(n int, username, mac, status, timeFrom, timeTo string) string {
	q := url.Values{}
	if username != "" {
		q.Set("username", username)
	}
	if mac != "" {
		q.Set("mac", mac)
	}
	if status != "" {
		q.Set("status", status)
	}
	if timeFrom != "" {
		q.Set("time_from", timeFrom)
	}
	if timeTo != "" {
		q.Set("time_to", timeTo)
	}
	q.Set("page", strconv.Itoa(n))
	return "/portal/auth-logs?" + q.Encode()
}

// portalAuthLogPageLinks 分页条目（复用 pageLink 结构）。
func portalAuthLogPageLinks(page, total int, username, mac, status, timeFrom, timeTo string) []pageLink {
	mk := func(n int) string { return portalAuthLogPageURL(n, username, mac, status, timeFrom, timeTo) }
	links := make([]pageLink, 0, 10)
	links = append(links, pageLink{Label: "«", URL: mk(page - 1), Disabled: page <= 1})
	start := page - 2
	if start < 1 {
		start = 1
	}
	end := page + 2
	if end > total {
		end = total
	}
	if start > 1 {
		links = append(links, pageLink{Label: "1", URL: mk(1)})
		if start > 2 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
	}
	for i := start; i <= end; i++ {
		links = append(links, pageLink{Label: strconv.Itoa(i), URL: mk(i), Active: i == page})
	}
	if end < total {
		if end < total-1 {
			links = append(links, pageLink{Label: "…", Disabled: true})
		}
		links = append(links, pageLink{Label: strconv.Itoa(total), URL: mk(total)})
	}
	links = append(links, pageLink{Label: "»", URL: mk(page + 1), Disabled: page >= total})
	return links
}

// portalAuthLogDetailPage 认证日志详情页视图。
type portalAuthLogDetailPage struct {
	pageBase
	Log       store.PortalAuthLog
	GetParams string // 美化后的 JSON（或原始文本）
}

// handlePortalAuthLogDetail 认证日志详情页（GET /portal/auth-logs/:id）。
func (a *App) handlePortalAuthLogDetail(c *gin.Context) {
	var l store.PortalAuthLog
	if err := a.DB.First(&l, "id = ?", c.Param("id")).Error; err != nil {
		c.Redirect(http.StatusFound, "/portal/auth-logs?err="+url.QueryEscape("日志不存在"))
		return
	}
	params := strings.TrimSpace(l.GetParams)
	if params != "" {
		var buf bytes.Buffer
		if err := json.Indent(&buf, []byte(params), "", "  "); err == nil {
			params = buf.String()
		}
	}
	u, _ := auth.CurrentUser(c)
	pg := portalAuthLogDetailPage{
		pageBase:  pageBase{ActiveMenu: "portal_auth_logs", CurrentUser: u, Config: a.Cfg},
		Log:       l,
		GetParams: params,
	}
	if rerr := a.Render.Render(c.Writer, "pages/portal/auth_log_detail", pg); rerr != nil {
		a.Log.Error("render portal auth log detail failed", "err", rerr)
	}
}
