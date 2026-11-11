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
	"net/url"
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
		"trunc":       trunc,
		"durSince":    durSince,
		"dstr":        dstr,
		"fsize":       fsize,
		"wkLabel":     wkLabel,
		"urlq":        url.QueryEscape,
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

// durSince 以人类可读格式输出自 t 起的时长（1天3小时 / 2小时5分 / 45秒）；t 为空返回 "-"。
func durSince(t any) string {
	var start time.Time
	switch v := t.(type) {
	case *time.Time:
		if v == nil {
			return "-"
		}
		start = *v
	case time.Time:
		start = v
	default:
		return "-"
	}
	if start.IsZero() {
		return "-"
	}
	d := time.Since(start)
	if d < 0 {
		return "-"
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%d天%d小时", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%d小时%d分", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%d分%d秒", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%d秒", int(d.Seconds()))
	}
}

// dstr 解引用字符串指针（nil → ""），便于模板渲染可选匹配条件。
func dstr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// fsize 人类可读文件大小。
func fsize(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.2f MB", float64(n)/(1024*1024))
	}
}

// wkLabel 将 weekdays "1,3" 转为 "周一三" 样式缩写（1=周一 … 7=周日）。
func wkLabel(w string) string {
	if w == "" {
		return ""
	}
	names := []string{"一", "二", "三", "四", "五", "六", "日"}
	var b strings.Builder
	b.WriteString("周")
	for _, part := range strings.Split(w, ",") {
		part = strings.TrimSpace(part)
		if len(part) != 1 || part[0] < '1' || part[0] > '7' {
			continue
		}
		b.WriteString(names[part[0]-'1'])
	}
	return b.String()
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

// trunc 截断字符串到前 n 个字符（超长时追加省略号），用于 SHA-256 等长指纹的缩略显示。
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
