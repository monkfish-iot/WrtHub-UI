// example.go 内置示例认证模板：以 go:embed 随二进制分发，
// 启动时由 server.seedPortalExamples 按 code 补齐缺失项（已存在的不覆盖）。
package portal

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed all:example-templates
var exampleTemplatesFS embed.FS

// ExampleTemplate 内置示例模板描述。
type ExampleTemplate struct {
	Code        string // 磁盘目录名（portal_templates.code）
	Name        string // 显示名称
	Description string
	EntryPage   string
	Dir         string // embed FS 内的子目录
}

// FS 返回该示例模板的只读文件系统（相对其子目录）。
func (ex ExampleTemplate) FS() fs.FS {
	sub, err := fs.Sub(exampleTemplatesFS, ex.Dir)
	if err != nil {
		// 编译期内置目录固定，不应失败
		return exampleTemplatesFS
	}
	return sub
}

// ExampleTemplates 返回全部内置示例模板。
func ExampleTemplates() []ExampleTemplate {
	return []ExampleTemplate{
		{
			Code:        "example-blue",
			Name:        "示例模板 · 清爽蓝",
			Description: "内置示例：蓝色渐变卡片式访客 WiFi 登录页，可复制后修改使用",
			EntryPage:   "index.html",
			Dir:         "example-templates/example-blue",
		},
		{
			Code:        "example-dark",
			Name:        "示例模板 · 暗夜紫",
			Description: "内置示例：深色玻璃拟态访客认证页，移动端优先布局",
			EntryPage:   "index.html",
			Dir:         "example-templates/example-dark",
		},
		{
			Code:        "wind",
			Name:        "内置模板 · 清风白",
			Description: "内置模板：轻盈通透的清风风格认证页，含接入信息与成功页会话详情",
			EntryPage:   "index.html",
			Dir:         "example-templates/wind",
		},
	}
}

// WriteDir 将 fsys 中全部文件写入模板目录（内置示例播种专用；
// 与 Extract 不同，来源为编译期内置 FS，无 zip 炸弹风险，仅做路径防御）。
func (s *Store) WriteDir(code string, fsys fs.FS) error {
	dir := s.Dir(code)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("创建模板目录失败: %w", err)
	}
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := path.Clean(strings.ReplaceAll(p, "\\", "/"))
		if strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") {
			return bad("非法的内置模板路径：" + p)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("读取内置模板文件失败: %w", err)
		}
		return os.WriteFile(dst, data, 0o640)
	})
}
