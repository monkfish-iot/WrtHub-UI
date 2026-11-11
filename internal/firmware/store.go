// Package firmware 固件库磁盘读写（设备「固件升级」+ 系统设置「固件管理」）。
//
// 固件按设备型号分目录存放：
//
//	<root>/<设备型号>/<固件文件>          如 ./firmware/MonkFish_AX835/ax835-v1.2.3.bin
//	<root>/<设备型号>/<固件主文件名>.json  同名说明文件（去掉固件扩展名后 + .json）
//
// 型号目录名映射：型号中的空白与文件系统非法字符统一替换为下划线
// （"MonkFish AX835" → "MonkFish_AX835"），保证目录名可安全用于路径拼接。
//
// 说明 JSON 契约（version 必填，其余可选）：
//
//	{"model":"MonkFish AX835","version":"v1.2.3","release_date":"2026-10-01","notes":"...","md5":"...","sha256":"..."}
//
// md5/sha256 存在时，升级下发前在服务端与文件实算值比对，不一致拒绝升级（固件库文件损坏防护）。
// 通过「固件管理」上传时，除 notes 外的字段均自动生成。
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
	"time"
)

// filenameRe 固件文件名白名单：字母数字开头，仅字母数字._-，防路径穿越与隐藏文件。
var filenameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var (
	// versionWithVRe 优先从文件名提取带 v 前缀的版本号（v1 / v1.2 / v1.2.3）。
	versionWithVRe = regexp.MustCompile(`(?i)v(\d+(?:\.\d+){0,3})`)
	// versionPlainRe 文件名无 v 前缀时回退提取纯数字版本号（1.2.3）。
	versionPlainRe = regexp.MustCompile(`\d+(?:\.\d+){1,3}`)
)

// ValidFilename 校验固件文件名合法（用于 API 入参防穿越）。
func ValidFilename(name string) bool {
	return filenameRe.MatchString(name) && !strings.Contains(name, "..")
}

// VersionFromFilename 从固件文件名推导版本号。
// 优先取 vX.Y.Z；无 v 前缀时取纯数字版本；都没有则回退文件名主干。
func VersionFromFilename(filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	if m := versionWithVRe.FindStringSubmatch(base); len(m) > 1 {
		return "v" + m[1]
	}
	if m := versionPlainRe.FindString(base); m != "" {
		return "v" + m
	}
	if base == "" {
		return filename
	}
	return base
}

// Desc 固件说明 JSON（与固件主文件名同名）。
type Desc struct {
	Model       string `json:"model,omitempty"`
	Version     string `json:"version"`
	ReleaseDate string `json:"release_date"`
	Notes       string `json:"notes"`
	MD5         string `json:"md5"`
	SHA256      string `json:"sha256"`
}

// Entry 列表条目（不含实算校验和，列表不做全文件读取）。
type Entry struct {
	Filename    string `json:"filename"`
	Model       string `json:"model,omitempty"`
	Version     string `json:"version"`
	ReleaseDate string `json:"release_date"`
	Notes       string `json:"notes"`
	SizeBytes   int64  `json:"size_bytes"`
	ModifiedAt  string `json:"modified_at"`
	HasChecksum bool   `json:"has_checksum"`
	// DescJSON 说明 JSON 原文（仅「固件管理」列表填充，设备页不展示）。
	DescJSON string `json:"-"`
}

// ModelGroup 按型号分组的固件列表（固件管理页使用）。
type ModelGroup struct {
	ModelDir string  // 磁盘目录名
	Model    string  // 展示用型号（优先说明 JSON 的 model 字段）
	Entries  []Entry // 该型号下的固件
}

// Store 固件库。
type Store struct {
	root     string
	maxBytes int64
}

// NewStore 创建固件库。root 为磁盘根目录，maxSizeMB 为单文件大小上限（≤0 用默认 512）。
func NewStore(root string, maxSizeMB int64) *Store {
	if maxSizeMB <= 0 {
		maxSizeMB = 512
	}
	return &Store{
		root:     strings.TrimRight(strings.TrimSpace(root), "/\\"),
		maxBytes: maxSizeMB * 1024 * 1024,
	}
}

// Root 返回固件库根目录（前端空态提示用）。
func (s *Store) Root() string { return s.root }

// ModelDirName 型号映射后的目录名（不含 root）。
// 空白与文件系统非法字符替换为下划线，确保结果可直接作为单级目录名使用。
func (s *Store) ModelDirName(model string) string {
	// 空白与非法字符统一折叠为单个下划线，避免出现连续下划线。
	m := strings.TrimSpace(model)
	var b strings.Builder
	lastUnderscore := false
	for _, r := range m {
		if isSafeDirRune(r) {
			if r == '_' {
				if lastUnderscore {
					continue
				}
				lastUnderscore = true
			} else {
				lastUnderscore = false
			}
			b.WriteRune(r)
			continue
		}
		if !lastUnderscore {
			b.WriteRune('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "._-")
	if len(out) > 128 {
		out = strings.Trim(out[:128], "._-")
	}
	if out == "" || out == "." || out == ".." {
		return ""
	}
	return out
}

// isSafeDirRune 目录名允许的字符（ASCII 字母数字与 ._-）。
func isSafeDirRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '-', r == '_':
		return true
	default:
		return false
	}
}

// resolveDirName 将调用方传入的型号解析为实际磁盘目录名。
// 若入参本身已是磁盘目录名（如「MonkFish_AX835」，固件管理页回传）则直接使用，
// 避免再次映射改写已有目录；否则按型号映射规则生成。
func (s *Store) resolveDirName(model string) string {
	mapped := s.ModelDirName(model)
	if mapped == "" {
		return ""
	}
	raw := strings.TrimSpace(model)
	if raw == "" || raw == "." || raw == ".." || strings.ContainsAny(raw, `/\`) {
		return mapped
	}
	if fi, err := os.Stat(filepath.Join(s.root, raw)); err == nil && fi.IsDir() {
		return raw
	}
	return mapped
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
	dirName := s.resolveDirName(model)
	if dirName == "" {
		return nil, nil
	}
	return s.listDir(dirName)
}

// ListAll 列出所有型号目录下的固件（按型号目录名排序），仅包含有可识别固件的目录。
func (s *Store) ListAll() ([]ModelGroup, error) {
	if s.root == "" {
		return nil, nil
	}
	items, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取固件库目录失败: %w", err)
	}
	groups := make([]ModelGroup, 0, len(items))
	for _, it := range items {
		if !it.IsDir() || strings.HasPrefix(it.Name(), ".") {
			continue
		}
		entries, err := s.listDir(it.Name())
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			continue
		}
		g := ModelGroup{ModelDir: it.Name(), Model: it.Name(), Entries: entries}
		for _, e := range entries {
			if e.Model != "" {
				g.Model = e.Model
				break
			}
		}
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ModelDir < groups[j].ModelDir })
	return groups, nil
}

// listDir 按实际磁盘目录名列出固件（不再次做型号映射，避免旧目录名被改写）。
func (s *Store) listDir(dirName string) ([]Entry, error) {
	dirName = strings.TrimSpace(dirName)
	if dirName == "" || dirName == "." || dirName == ".." || strings.ContainsAny(dirName, `/\`) {
		return nil, nil
	}
	dir := filepath.Join(s.root, dirName)
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取固件目录失败: %w", err)
	}
	out := make([]Entry, 0, len(items))
	for _, it := range items {
		if it.IsDir() || strings.HasPrefix(it.Name(), ".") || strings.HasSuffix(strings.ToLower(it.Name()), ".json") {
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
		desc, _, err := s.loadDescInDir(dirName, it.Name())
		if err != nil || desc.Version == "" {
			continue
		}
		descJSON, _ := json.MarshalIndent(desc, "", "  ")
		out = append(out, Entry{
			Filename:    it.Name(),
			Model:       desc.Model,
			Version:     desc.Version,
			ReleaseDate: desc.ReleaseDate,
			Notes:       desc.Notes,
			SizeBytes:   fi.Size(),
			ModifiedAt:  fi.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
			HasChecksum: desc.MD5 != "" || desc.SHA256 != "",
			DescJSON:    string(descJSON),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

func (s *Store) loadDescInDir(dirName, filename string) (*Desc, string, error) {
	jsonPath := s.descPathInDir(dirName, filename)
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
	return s.descPathInDir(s.resolveDirName(model), filename)
}

func (s *Store) descPathInDir(dirName, filename string) string {
	base := filename
	if ext := filepath.Ext(filename); ext != "" {
		base = strings.TrimSuffix(filename, ext)
	}
	return filepath.Join(s.root, dirName, base+".json")
}

// GetDesc 读取固件说明 JSON。
func (s *Store) GetDesc(model, filename string) (*Desc, error) {
	dirName := s.resolveDirName(model)
	if dirName == "" {
		return nil, fmt.Errorf("设备型号为空，无法定位固件目录")
	}
	if !ValidFilename(filename) {
		return nil, fmt.Errorf("固件文件名非法")
	}
	desc, _, err := s.loadDescInDir(dirName, filename)
	return desc, err
}

// Open 打开固件文件（升级下发用），返回文件句柄、大小与说明。
func (s *Store) Open(model, filename string) (*os.File, int64, *Desc, error) {
	f, fi, desc, err := s.OpenFile(model, filename)
	if err != nil {
		return nil, 0, nil, err
	}
	return f, fi.Size(), desc, nil
}

// OpenFile 打开固件文件并返回文件信息（下载用）。
func (s *Store) OpenFile(model, filename string) (*os.File, os.FileInfo, *Desc, error) {
	if !ValidFilename(filename) {
		return nil, nil, nil, fmt.Errorf("固件文件名非法")
	}
	dirName := s.resolveDirName(model)
	if dirName == "" {
		return nil, nil, nil, fmt.Errorf("设备型号为空，无法定位固件目录")
	}
	fwPath := filepath.Join(s.root, dirName, filename)
	fi, err := os.Stat(fwPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("固件文件不存在: %s", filename)
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, nil, fmt.Errorf("固件路径非法")
	}
	if s.maxBytes > 0 && fi.Size() > s.maxBytes {
		return nil, nil, nil, fmt.Errorf("固件文件超过大小上限（%d MB）", s.maxBytes/1024/1024)
	}
	desc, _, err := s.loadDescInDir(dirName, filename)
	if err != nil {
		return nil, nil, nil, err
	}
	if desc.Version == "" {
		return nil, nil, nil, fmt.Errorf("说明文件缺少 version 字段")
	}
	f, err := os.Open(fwPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("打开固件文件失败: %w", err)
	}
	return f, fi, desc, nil
}

// Save 保存上传的固件并自动生成同名说明 JSON。
// version 由文件名推导，release_date 取上传当天，md5/sha256 实算，notes 由用户填写。
// 同名固件已存在时拒绝覆盖，避免误传覆盖线上正在使用的固件。
func (s *Store) Save(model, filename, notes string, r io.Reader, size int64) (*Desc, error) {
	dirName := s.resolveDirName(model)
	if dirName == "" {
		return nil, fmt.Errorf("硬件型号为空或包含非法字符")
	}
	filename = strings.TrimSpace(filename)
	if !ValidFilename(filename) {
		return nil, fmt.Errorf("固件文件名非法：仅允许字母、数字、点、下划线和短横线")
	}
	if strings.HasSuffix(strings.ToLower(filename), ".json") {
		return nil, fmt.Errorf("不能上传 .json 说明文件")
	}
	if size <= 0 {
		return nil, fmt.Errorf("固件文件为空")
	}
	if s.maxBytes > 0 && size > s.maxBytes {
		return nil, fmt.Errorf("固件文件超过大小上限（%d MB）", s.maxBytes/1024/1024)
	}

	dir := filepath.Join(s.root, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建固件目录失败: %w", err)
	}
	fwPath := filepath.Join(dir, filename)
	if _, err := os.Stat(fwPath); err == nil {
		return nil, fmt.Errorf("同名固件已存在：%s（请先删除或改名后再上传）", filename)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("检查固件文件失败: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	h5 := md5.New()
	h2 := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, h5, h2), r)
	closeErr := tmp.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("写入固件文件失败: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("关闭固件临时文件失败: %w", closeErr)
	}
	if written != size {
		return nil, fmt.Errorf("固件内容不完整（收到 %d 字节，期望 %d 字节）", written, size)
	}
	if s.maxBytes > 0 && written > s.maxBytes {
		return nil, fmt.Errorf("固件文件超过大小上限（%d MB）", s.maxBytes/1024/1024)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return nil, fmt.Errorf("设置固件文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, fwPath); err != nil {
		return nil, fmt.Errorf("保存固件文件失败: %w", err)
	}
	committed = true

	desc := &Desc{
		Model:       strings.TrimSpace(model),
		Version:     VersionFromFilename(filename),
		ReleaseDate: time.Now().Format("2006-01-02"),
		Notes:       strings.TrimSpace(notes),
		MD5:         hex.EncodeToString(h5.Sum(nil)),
		SHA256:      hex.EncodeToString(h2.Sum(nil)),
	}
	if err := s.writeDescInDir(dirName, filename, desc); err != nil {
		_ = os.Remove(fwPath)
		return nil, fmt.Errorf("生成说明 JSON 失败: %w", err)
	}
	return desc, nil
}

// UpdateNotes 更新固件备注，并重新写入同名说明 JSON（其余字段保持不变）。
func (s *Store) UpdateNotes(model, filename, notes string) (*Desc, error) {
	dirName := s.resolveDirName(model)
	if dirName == "" {
		return nil, fmt.Errorf("硬件型号为空或包含非法字符")
	}
	if !ValidFilename(filename) {
		return nil, fmt.Errorf("固件文件名非法")
	}
	fwPath := filepath.Join(s.root, dirName, filename)
	if fi, err := os.Stat(fwPath); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("固件文件不存在：%s", filename)
	}
	desc, _, err := s.loadDescInDir(dirName, filename)
	if err != nil {
		return nil, err
	}
	desc.Notes = strings.TrimSpace(notes)
	if err := s.writeDescInDir(dirName, filename, desc); err != nil {
		return nil, fmt.Errorf("写入说明 JSON 失败: %w", err)
	}
	return desc, nil
}

// Delete 删除固件文件及其同名说明 JSON。
func (s *Store) Delete(model, filename string) error {
	dirName := s.resolveDirName(model)
	if dirName == "" {
		return fmt.Errorf("硬件型号为空或包含非法字符")
	}
	if !ValidFilename(filename) {
		return fmt.Errorf("固件文件名非法")
	}
	fwPath := filepath.Join(s.root, dirName, filename)
	if err := os.Remove(fwPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("固件文件不存在：%s", filename)
		}
		return fmt.Errorf("删除固件文件失败: %w", err)
	}
	if err := os.Remove(s.descPathInDir(dirName, filename)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除说明 JSON 失败: %w", err)
	}
	return nil
}

// writeDescInDir 原子写入说明 JSON（先临时文件再 rename）。
func (s *Store) writeDescInDir(dirName, filename string, d *Desc) error {
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path := s.descPathInDir(dirName, filename)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".desc-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
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
