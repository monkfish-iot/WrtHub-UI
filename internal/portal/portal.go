// Package portal 提供 PORTAL 认证模板的文件层能力：zip 包校验/解压落盘、
// 磁盘目录复制（fork）、文件清单扫描与安全路径解析。
//
// 目录布局：{TemplateRoot}/{code}/...，code 为模板磁盘目录名（DB portal_templates.code）。
// 约束对齐 tentant-mng 单租户化裁剪：zip ≤ 10MB、解压后总量 ≤ 30MB、文件数 ≤ 200、
// 扩展名白名单、防路径穿越与符号链接逃逸。
package portal

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// 模板 zip 包限制（防解压炸弹与路径注入）与在线编辑限制。
const (
	MaxZipBytes   = 10 << 20 // zip 压缩包 ≤ 10MB
	MaxTotalBytes = 30 << 20 // 解压后总大小 ≤ 30MB
	MaxFiles      = 200
	MaxEditBytes  = 512 << 10 // 单文件在线编辑内容 ≤ 512KB
)

// textExts 允许在线编辑的文本类扩展名（zip 白名单的文本子集）。
var textExts = map[string]bool{
	".html": true, ".htm": true, ".css": true, ".js": true, ".mjs": true,
	".json": true, ".xml": true, ".txt": true,
}

// IsTextExt 判断文件是否为可在线编辑的文本类文件。
func IsTextExt(name string) bool {
	return textExts[strings.ToLower(path.Ext(name))]
}

// CleanRelPath 清洗并校验模板内相对路径（不要求文件存在），返回 slash 相对路径。
func CleanRelPath(relPath string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(relPath, "\\", "/"))
	if clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", bad("非法的模板文件路径")
	}
	return clean, nil
}

// extWhitelist 模板内允许的文件扩展名（静态资源 + 配置描述文件）。
var extWhitelist = map[string]bool{
	".html": true, ".htm": true, ".css": true, ".js": true, ".mjs": true,
	".json": true, ".xml": true, ".txt": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".ico": true, ".webp": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp3": true, ".mp4": true,
}

// Store PORTAL 模板磁盘存储。
// root 运行时可经 SetRoot 热更新（系统设置页），读写经 mu 保护。
type Store struct {
	mu   sync.RWMutex
	root string // 模板根目录
}

// NewStore 创建存储并确保根目录存在。
func NewStore(root string) (*Store, error) {
	if root == "" {
		root = "./data/portal-templates"
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create portal template root: %w", err)
	}
	return &Store{root: root}, nil
}

// Root 返回当前模板根目录。
func (s *Store) Root() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.root
}

// SetRoot 热更新模板根目录（确保目录存在；原目录下模板文件不自动迁移）。
func (s *Store) SetRoot(root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return errors.New("portal template root cannot be empty")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("create portal template root: %w", err)
	}
	s.mu.Lock()
	s.root = root
	s.mu.Unlock()
	return nil
}

// Dir 返回模板 code 对应的磁盘目录。
func (s *Store) Dir(code string) string { return filepath.Join(s.Root(), code) }

// TemplateFile 是模板包内单个文件（相对路径 + 大小 + 是否文本类）。
type TemplateFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Text bool   `json:"text"` // 可在线编辑（文本类）
}

// ValidationError 业务校验错误（提示给用户）。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func bad(msg string) error { return &ValidationError{Msg: msg} }

// ValidateZipReader 校验 zip 内容（大小/文件数/扩展名/路径安全），返回 (文件数, 解压后总大小)。
func ValidateZipReader(r io.ReaderAt, size int64) (int, int64, error) {
	return validateZip(r, size)
}

// validateZip 校验 zip 内容（大小/文件数/扩展名/路径安全），返回 (文件数, 解压后总大小)。
func validateZip(r io.ReaderAt, size int64) (int, int64, error) {
	if size > MaxZipBytes {
		return 0, 0, bad("模板包超过 10MB 限制")
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return 0, 0, bad("无法读取 zip 包：" + err.Error())
	}
	var total int64
	var count int
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// 路径安全：拒绝绝对路径与 .. 穿越（统一 slash 后检查每段）。
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") {
			return 0, 0, bad("模板包含非法路径：" + f.Name)
		}
		ext := strings.ToLower(path.Ext(name))
		if !extWhitelist[ext] {
			return 0, 0, bad("模板包含不允许的文件类型：" + f.Name)
		}
		if f.UncompressedSize64 > MaxTotalBytes {
			return 0, 0, bad("模板包含超大文件：" + f.Name)
		}
		total += int64(f.UncompressedSize64)
		count++
		if count > MaxFiles {
			return 0, 0, bad("模板包文件数超过 200 限制")
		}
	}
	if total > MaxTotalBytes {
		return 0, 0, bad("模板包解压后总大小超过 30MB 限制")
	}
	if count == 0 {
		return 0, 0, bad("模板包为空")
	}
	return count, total, nil
}

// Extract 解压 zip 到 dir（调用前已 validateZip；此处再次逐项防御）。
func (s *Store) Extract(code string, r io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return bad("无法读取 zip 包：" + err.Error())
	}
	dir := s.Dir(code)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("创建模板目录失败: %w", err)
	}
	for _, f := range zr.File {
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(name)), 0o750); err != nil {
				return err
			}
			continue
		}
		// 二次校验（防 TOCTOU 式构造）
		if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || !extWhitelist[strings.ToLower(path.Ext(name))] {
			return bad("模板包含非法条目：" + f.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if err := extractFile(f, dst); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(f *zip.File, dst string) error {
	src, err := f.Open()
	if err != nil {
		return fmt.Errorf("打开模板条目失败: %w", err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("写入模板文件失败: %w", err)
	}
	defer out.Close()
	// 限量拷贝，防声明大小与实际不符的解压炸弹
	if _, err := io.Copy(out, io.LimitReader(src, MaxTotalBytes+1)); err != nil {
		return fmt.Errorf("解压模板文件失败: %w", err)
	}
	return nil
}

// HasEntry 检查模板包内是否存在指定入口文件（相对 slash 路径）。
func (s *Store) HasEntry(code, entryPage string) bool {
	st, err := os.Stat(filepath.Join(s.Dir(code), filepath.FromSlash(path.Clean(entryPage))))
	return err == nil && !st.IsDir()
}

// HasSchemaFile 检查模板目录是否含 schema.json。
func (s *Store) HasSchemaFile(code string) bool {
	st, err := os.Stat(filepath.Join(s.Dir(code), "schema.json"))
	return err == nil && !st.IsDir()
}

// Files 扫描模板目录，返回相对 slash 路径与大小（按路径排序）。
func (s *Store) Files(code string) ([]TemplateFile, error) {
	base := s.Dir(code)
	files := make([]TemplateFile, 0, 16)
	err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return rerr
		}
		info, serr := d.Info()
		if serr != nil {
			return serr
		}
		files = append(files, TemplateFile{Path: filepath.ToSlash(rel), Size: info.Size(), Text: IsTextExt(rel)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// Resolve 安全解析模板内相对文件路径，返回绝对路径（防穿越；不存在返回错误）。
func (s *Store) Resolve(code, relPath string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(relPath, "\\", "/"))
	if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", bad("非法的模板文件路径")
	}
	abs := filepath.Join(s.Dir(code), filepath.FromSlash(clean))
	// 确认仍在模板目录内
	base, err := filepath.Abs(s.Dir(code))
	if err != nil {
		return "", err
	}
	absAbs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absAbs, base+string(filepath.Separator)) {
		return "", bad("非法的模板文件路径")
	}
	st, err := os.Stat(absAbs)
	if err != nil {
		return "", errors.New("模板文件不存在")
	}
	if st.IsDir() {
		return "", errors.New("模板文件不可读")
	}
	return absAbs, nil
}

// ContentType 按扩展名推断 Content-Type。
func ContentType(name string) string {
	if ct := mime.TypeByExtension(strings.ToLower(path.Ext(name))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// Remove 删除模板磁盘目录（DB 删除后调用；目录不存在视为成功）。
func (s *Store) Remove(code string) error {
	if err := os.RemoveAll(s.Dir(code)); err != nil {
		return fmt.Errorf("删除模板目录失败: %w", err)
	}
	return nil
}

// Fork 复制模板目录（srcCode → dstCode）。
func (s *Store) Fork(srcCode, dstCode string) error {
	return os.CopyFS(s.Dir(dstCode), os.DirFS(s.Dir(srcCode)))
}

// ReadFile 读取模板内文件内容（Resolve 负责路径安全）。
func (s *Store) ReadFile(code, relPath string) ([]byte, error) {
	abs, err := s.Resolve(code, relPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// WriteFile 写入模板内文件（父目录自动创建；调用方需先做扩展名/大小校验）。
func (s *Store) WriteFile(code, relPath string, data []byte) error {
	clean, err := CleanRelPath(relPath)
	if err != nil {
		return err
	}
	dst := filepath.Join(s.Dir(code), filepath.FromSlash(clean))
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("创建模板目录失败: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o640); err != nil {
		return fmt.Errorf("写入模板文件失败: %w", err)
	}
	return nil
}

// DeleteFile 删除模板内单个文件（不存在视为成功）。
func (s *Store) DeleteFile(code, relPath string) error {
	clean, err := CleanRelPath(relPath)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.Dir(code), filepath.FromSlash(clean))); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除模板文件失败: %w", err)
	}
	return nil
}
