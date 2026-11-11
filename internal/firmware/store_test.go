package firmware

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionFromFilename(t *testing.T) {
	cases := map[string]string{
		"monkfish-v0.0.6-mediatek-filogic-monkfish_ax835-squashfs-sysupgrade.bin": "v0.0.6",
		"fw-1.2.3.bin":         "v1.2.3",
		"firmware-v2.img":      "v2",
		"monkfish_ax835.bin":   "monkfish_ax835",
		"MonkFish_AX835_v1.0.tar.gz": "v1.0",
	}
	for name, want := range cases {
		if got := VersionFromFilename(name); got != want {
			t.Errorf("VersionFromFilename(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestModelDirName(t *testing.T) {
	s := NewStore("root", 1)
	cases := map[string]string{
		"MonkFish AX835":     "MonkFish_AX835",
		"  MonkFish   AX835 ": "MonkFish_AX835",
		"../../etc/passwd":    "etc_passwd",
		"a/b":                "a_b",
		".":                  "",
		"":                   "",
		"AX835 (v2)":         "AX835_v2",
	}
	for in, want := range cases {
		if got := s.ModelDirName(in); got != want {
			t.Errorf("ModelDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveListNotesDelete(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root, 1)
	payload := []byte("fake-firmware-content")
	filename := "monkfish-v0.0.6-test.bin"

	desc, err := s.Save("MonkFish AX835", filename, "首版发布", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if desc.Model != "MonkFish AX835" {
		t.Errorf("desc.Model = %q", desc.Model)
	}
	if desc.Version != "v0.0.6" {
		t.Errorf("desc.Version = %q, want v0.0.6", desc.Version)
	}
	if desc.ReleaseDate == "" || desc.MD5 == "" || desc.SHA256 == "" {
		t.Errorf("自动生成字段缺失: %+v", desc)
	}

	fwPath := filepath.Join(root, "MonkFish_AX835", filename)
	if _, err := os.Stat(fwPath); err != nil {
		t.Fatalf("固件文件未落盘: %v", err)
	}
	jsonPath := filepath.Join(root, "MonkFish_AX835", "monkfish-v0.0.6-test.json")
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("说明 JSON 未生成: %v", err)
	}
	var onDisk Desc
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("说明 JSON 解析失败: %v", err)
	}
	if onDisk.Notes != "首版发布" || onDisk.Model != "MonkFish AX835" {
		t.Errorf("说明 JSON 内容不符: %+v", onDisk)
	}

	groups, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Entries) != 1 {
		t.Fatalf("ListAll 结果 = %+v", groups)
	}
	g := groups[0]
	if g.Model != "MonkFish AX835" || g.ModelDir != "MonkFish_AX835" {
		t.Errorf("分组信息不符: %+v", g)
	}
	if !strings.Contains(g.Entries[0].DescJSON, "首版发布") {
		t.Errorf("DescJSON 未包含备注: %s", g.Entries[0].DescJSON)
	}

	if _, err := s.UpdateNotes("MonkFish AX835", filename, "修复已知问题"); err != nil {
		t.Fatalf("UpdateNotes: %v", err)
	}
	d, err := s.GetDesc("MonkFish AX835", filename)
	if err != nil {
		t.Fatalf("GetDesc: %v", err)
	}
	if d.Notes != "修复已知问题" || d.Version != "v0.0.6" || d.MD5 != desc.MD5 {
		t.Errorf("UpdateNotes 后字段不符: %+v", d)
	}

	f, _, _, err := s.Open("MonkFish AX835", filename)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ok, err := VerifyChecksum(f, d)
	_ = f.Close()
	if err != nil || !ok {
		t.Fatalf("VerifyChecksum = %v, %v", ok, err)
	}

	if err := s.Delete("MonkFish AX835", filename); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(fwPath); !os.IsNotExist(err) {
		t.Errorf("固件文件删除后仍存在: %v", err)
	}
	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Errorf("说明 JSON 删除后仍存在: %v", err)
	}
	groups, err = s.ListAll()
	if err != nil || len(groups) != 0 {
		t.Errorf("删除后 ListAll = %+v, err = %v", groups, err)
	}
}

func TestSaveRejectsDuplicateAndInvalid(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root, 1)
	payload := []byte("x")
	if _, err := s.Save("MonkFish AX835", "fw-v1.0.bin", "", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("首次 Save: %v", err)
	}
	if _, err := s.Save("MonkFish AX835", "fw-v1.0.bin", "", bytes.NewReader(payload), int64(len(payload))); err == nil {
		t.Errorf("同名固件应拒绝覆盖")
	}
	if _, err := s.Save("", "fw.bin", "", bytes.NewReader(payload), int64(len(payload))); err == nil {
		t.Errorf("空型号应拒绝上传")
	}
	if _, err := s.Save("MonkFish AX835", "../evil.bin", "", bytes.NewReader(payload), int64(len(payload))); err == nil {
		t.Errorf("路径穿越文件名应拒绝")
	}
	if _, err := s.Save("MonkFish AX835", "fw.json", "", bytes.NewReader(payload), int64(len(payload))); err == nil {
		t.Errorf(".json 文件应拒绝上传")
	}
}
