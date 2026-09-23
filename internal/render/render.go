// Package render 提供 HTML 模板渲染。
//
// 设计（见开发技术文档 §7/§8）：
//   - 模板用 Go html/template，按页面拆分；
//   - 解析时以 layout.html 为骨架，各页面文件 override "content" block；
//   - 外部目录优先（用户可改风格）；todo8 补 go:embed 兜底。
//
// page name 为相对 templates_dir 的路径（去掉 .html，分隔符统一为 /），
// 例如 pages/devices/list。
package render

import (
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Renderer 持有预解析的页面模板集合。
type Renderer struct {
	pages map[string]*template.Template
}

// NewRenderer 解析 dir 下所有 *.html（layout.html 作为骨架，其余为页面）。
// 每个页面文件与 layout 一起 ParseFiles，注册名为其相对路径（去 .html）。
func NewRenderer(dir string) (*Renderer, error) {
	layout := filepath.Join(dir, "layout.html")
	if _, err := os.Stat(layout); err != nil {
		return nil, fmt.Errorf("layout template not found under %s: %w", dir, err)
	}
	r := &Renderer{pages: map[string]*template.Template{}}
	funcs := template.FuncMap{
		"tFmt":        tFmt,
		"statusBadge": statusBadge,
		"bsstr":       bsstr,
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".html") {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		if rel == "layout.html" {
			return nil
		}
		t, perr := template.New("").Funcs(funcs).ParseFiles(layout, path)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		name := strings.TrimSuffix(filepath.ToSlash(rel), ".html")
		r.pages[name] = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(r.pages) == 0 {
		return nil, fmt.Errorf("no page templates found under %s", dir)
	}
	return r, nil
}

// Render 渲染指定 page 到 w。
func (r *Renderer) Render(w io.Writer, page string, data any) error {
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("unknown page template: %s", page)
	}
	return t.ExecuteTemplate(w, "layout.html", data)
}

// tFmt 格式化时间；接受 *time.Time 或 time.Time；nil/零值返回 "-"。
func tFmt(t any) string {
	switch v := t.(type) {
	case nil:
		return "-"
	case *time.Time:
		if v == nil || v.IsZero() {
			return "-"
		}
		return v.Format("2006-01-02 15:04")
	case time.Time:
		if v.IsZero() {
			return "-"
		}
		return v.Format("2006-01-02 15:04")
	default:
		return "-"
	}
}

// statusBadge 将设备状态映射为 Bootstrap badge 颜色类后缀（success/secondary/warning）。
func statusBadge(s string) string {
	switch strings.ToLower(s) {
	case "online":
		return "success"
	case "offline":
		return "secondary"
	case "pending":
		return "warning"
	default:
		return "secondary"
	}
}

// bsstr 将 []byte 转 string（用于审计日志 Detail 字段渲染）。
func bsstr(b []byte) string {
	return string(b)
}
