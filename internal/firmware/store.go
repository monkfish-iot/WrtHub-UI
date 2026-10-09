// Package firmware 固件库磁盘读取（设备「固件升级」）。
//
// 固件由运维放置在服务器磁盘（不做页面上传），按设备型号分目录：
//
//	<root>/<设备型号>/<固件文件>          如 ./firmware/MonkFish_AX835/ax835-v1.2.3.bin
//	<root>/<设备型号>/<固件主文件名>.json  同名说明文件（去掉固件扩展名后 + .json）
//
// 型号目录名映射：设备型号中的空白统一替换为下划线（"MonkFish AX835" → "MonkFish_AX835"）。
//
// 说明 JSON 契约（version 必填，其余可选）：
//
//	{"version":"v1.2.3","release_date":"2026-10-01","notes":"...","md5":"...","sha256":"..."}
//
// md5/sha256 存在时，升级下发前在服务端与文件实算值比对，不一致拒绝升级（固件库文件损坏防护）。
package firmware

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// filenameRe 固件文件名白名单：字母数字开头，仅字母数字._-，防路径穿越与隐藏文件。
var filenameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidFilename 校验固件文件名合法（用于 API 入参防穿越）。
func ValidFilename(name string) bool {
	return filenameRe.MatchString(name) && !strings.Contains(name, "..")
}

// Desc 固件说明 JSON（与固件主文件名同名）。
type Desc struct {
	Version     string `json:"version"`
	ReleaseDate string `json:"release_date"`
	Notes       string `json:"notes"`
	MD5         string `json:"md5"`
	SHA256      string `json:"sha256"`
}

// Entry 列表条目（不含实算校验和，列表不做全文件读取）。
type Entry struct {
	Filename    string `json:"filename"`
	Version     string `json:"version"`
	ReleaseDate string `json:"release_date"`
	Notes       string `json:"notes"`
	SizeBytes   int64  `json:"size_bytes"`
	ModifiedAt  string `json:"modified_at"`
	HasChecksum bool   `json:"has_checksum"`
}

// Store 固件库。
type Store struct {
	root      string
	maxBytes  int64
	modelFold *regexp.Regexp
}

// NewStore 创建固件库。root 为磁盘根目录，maxSizeMB 为单文件大小上限（≤0 用默认 512）。
func NewStore(root string, maxSizeMB int64) *Store {
	if maxSizeMB <= 0 {
		maxSizeMB = 512
	}
	return &Store{
		root:      strings.TrimRight(strings.TrimSpace(root), "/\\"),
		maxBytes:  maxSizeMB * 1024 * 1024,
		modelFold: regexp.MustCompile(`\s+`),
	}
}

// Root 返回固件库根目录（前端空态提示用）。
func (s *Store) Root() string { return s.root }

// ModelDirName 型号映射后的目录名（不含 root）：内部空白统一替换为下划线。
func (s *Store) ModelDirName(model string) string {
	m := s.modelFold.ReplaceAllString(strings.TrimSpace(model), "_")
	m = strings.Trim(m, "/\\.")
	if m == "" || m == "." || m == "_" {
		return ""
	}
	return m
}

// ModelDir 型号对应目录的磁盘路径。
func (s *Store) ModelDir(model string) string {
	m := s.ModelDirName(model)
	if m == "" {
		return ""
	}
	return filepath.Join(s.root, m)
}

// List 列出某型号目录下的固件条目（按文件名排序）。目录不存在/为空返回空切片。
func (s *Store) List(model string) ([]Entry, error) {
	dir := s.ModelDir(model)
	if dir == "" {
		return nil, nil
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取固件目录失败: %w", err)
	}
	out := make([]Entry, 0, len(items))
	for _, it := range items {
		if it.IsDir() || strings.HasPrefix(it.Name(), ".") || strings.HasSuffix(it.Name(), ".json") {
			continue
		}
		if !ValidFilename(it.Name()) {
			continue
		}
		fwPath := filepath.Join(dir, it.Name())
		fi, err := os.Stat(fwPath)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if s.maxBytes > 0 && fi.Size() > s.maxBytes {
			continue
		}
		desc, _, err := s.loadDesc(model, it.Name())
		if err != nil || desc.Version == "" {
			continue
		}
		out = append(out, Entry{
			Filename:    it.Name(),
			Version:     desc.Version,
			ReleaseDate: desc.ReleaseDate,
			Notes:       desc.Notes,
			SizeBytes:   fi.Size(),
			ModifiedAt:  fi.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
			HasChecksum: desc.MD5 != "" || desc.SHA256 != "",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

func (s *Store) loadDesc(model, filename string) (*Desc, string, error) {
	jsonPath := s.DescPath(model, filename)
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, jsonPath, fmt.Errorf("读取说明文件失败: %w", err)
	}
	var d Desc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, jsonPath, fmt.Errorf("说明文件 JSON 解析失败: %w", err)
	}
	return &d, jsonPath, nil
}

// DescPath 说明文件路径（固件主文件名去扩展名 + .json）。
func (s *Store) DescPath(model, filename string) string {
	base := filename
	if ext := filepath.Ext(filename); ext != "" {
		base = strings.TrimSuffix(filename, ext)
	}
	return filepath.Join(s.ModelDir(model), base+".json")
}

// Open 打开固件文件（升级下发用），返回文件句柄、大小与说明。
func (s *Store) Open(model, filename string) (*os.File, int64, *Desc, error) {
	if !ValidFilename(filename) {
		return nil, 0, nil, fmt.Errorf("固件文件名非法")
	}
	dir := s.ModelDir(model)
	if dir == "" {
		return nil, 0, nil, fmt.Errorf("设备型号为空，无法定位固件目录")
	}
	fwPath := filepath.Join(dir, filename)
	fi, err := os.Stat(fwPath)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("固件文件不存在: %s", filename)
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, nil, fmt.Errorf("固件路径非法")
	}
	if s.maxBytes > 0 && fi.Size() > s.maxBytes {
		return nil, 0, nil, fmt.Errorf("固件文件超过大小上限（%d MB）", s.maxBytes/1024/1024)
	}
	desc, _, err := s.loadDesc(model, filename)
	if err != nil {
		return nil, 0, nil, err
	}
	if desc.Version == "" {
		return nil, 0, nil, fmt.Errorf("说明文件缺少 version 字段")
	}
	f, err := os.Open(fwPath)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("打开固件文件失败: %w", err)
	}
	return f, fi.Size(), desc, nil
}

// VerifyChecksum 实算文件 md5/sha256 并与说明 JSON 声明值比对。
func VerifyChecksum(f *os.File, desc *Desc) (ok bool, err error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	h5 := md5.New()
	h2 := sha256.New()
	if _, err := io.Copy(io.MultiWriter(h5, h2), f); err != nil {
		return false, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	wantMD5 := strings.ToLower(strings.TrimSpace(desc.MD5))
	wantSHA := strings.ToLower(strings.TrimSpace(desc.SHA256))
	if wantMD5 != "" && hex.EncodeToString(h5.Sum(nil)) != wantMD5 {
		return false, nil
	}
	if wantSHA != "" && hex.EncodeToString(h2.Sum(nil)) != wantSHA {
		return false, nil
	}
	return true, nil
}

// FileMD5Hex 计算文件 md5。
func FileMD5Hex(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	_, _ = f.Seek(0, io.SeekStart)
	return hex.EncodeToString(h.Sum(nil))
}
