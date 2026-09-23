// Command pack 打 tar.gz 部署包，确保 Linux 解压后二进制带 +x 执行位。
//
// 背景（开发技术文档 §8）：Windows NTFS 不存 Unix mode，交叉编译产物传到 Linux
// 后默认没有执行位；本机 bsdtar 的 --mode 选项在 Windows 版上不可用。
// 故用 Go archive/tar 主动写 header.Mode：
//   - 名字以 "wrthub-ui" 结尾的文件 → 0755（可执行）
//   - 其他文件 → 0644
//
// 用法：
//
//	go run ./tools/pack -out dist/wrthub-ui-linux-amd64.tar.gz -base dist dist/wrthub-ui dist/config.yaml dist/data
//
// -base 指定归档名相对的目录，使归档内路径为 wrthub-ui / config.yaml / data/...
// 而非 dist/...。build.ps1 在交叉编译后调用本工具生成部署包。
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	out := flag.String("out", "wrthub-ui.tar.gz", "output tar.gz path")
	base := flag.String("base", ".", "archive names are computed relative to this dir")
	flag.Parse()
	entries := flag.Args()
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./tools/pack -out X.tar.gz [-base dir] path [path ...]")
		os.Exit(2)
	}

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create output:", err)
		os.Exit(1)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	for _, root := range entries {
		info, err := os.Stat(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "stat", root, ":", err)
			os.Exit(1)
		}
		if info.IsDir() {
			if err := filepath.Walk(root, func(p string, fi os.FileInfo, werr error) error {
				if werr != nil {
					return werr
				}
				if fi.IsDir() {
					return nil
				}
				return addFile(tw, p, relName(*base, p))
			}); err != nil {
				fmt.Fprintln(os.Stderr, "walk", root, ":", err)
				os.Exit(1)
			}
		} else {
			if err := addFile(tw, root, relName(*base, root)); err != nil {
				fmt.Fprintln(os.Stderr, "add", root, ":", err)
				os.Exit(1)
			}
		}
	}
}

// addFile 把 fsPath 加入 tar，archiveName 为归档内路径（统一 / 分隔）。
// 名字以 "wrthub-ui" 结尾的视为可执行，mode 0755；否则 0644。
func addFile(tw *tar.Writer, fsPath, archiveName string) error {
	body, err := os.ReadFile(fsPath)
	if err != nil {
		return err
	}
	archiveName = filepath.ToSlash(archiveName)
	mode := int64(0o644)
	if strings.HasSuffix(archiveName, "wrthub-ui") {
		mode = 0o755
	}
	hdr := &tar.Header{
		Name:     archiveName,
		Mode:     mode,
		Size:     int64(len(body)),
		ModTime:  modTimeFor(fsPath),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, bytes.NewReader(body))
	return err
}

// relName 计算 p 相对 base 的归档名，失败时回退为 p 自身（去掉 ./ 前缀）。
func relName(base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return strings.TrimPrefix(filepath.ToSlash(p), "./")
	}
	return filepath.ToSlash(rel)
}

// modTimeFor 取文件 mtime；失败返回零值。
func modTimeFor(fsPath string) time.Time {
	if fi, err := os.Stat(fsPath); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
