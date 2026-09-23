// Package assets 内置静态资源（Bootstrap 5 / Font Awesome 6 / htmx），见开发技术文档 §8。
//
// 打包策略：外部优先 + embed 兜底。
//   - 运行时优先读 cfg.Assets.ExternalDir（默认 ./assets，用户可改风格）；
//   - 该目录下文件不存在时回退到本包内置的 go:embed 资源。
//
// 资源以 files/ 为根目录，导出 FS() 已 fs.Sub 到 files/ 内容层，
// 即返回的 fs.FS 中路径形如 css/bootstrap.min.css、js/htmx.min.js、
// webfonts/fa-solid-900.woff2，与外部 ./assets/ 目录的相对路径一致，
// 便于 server.registerAssetsRoutes 统一查找。
package assets

import (
	"embed"
	"io/fs"
)

//go:embed all:files
var embedded embed.FS

// FS 返回已 sub 到 files/ 内容层的静态资源 FS。
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "files")
	if err != nil {
		// 仅在 //go:embed 指令写错时发生，编译期即可发现；运行时 panic 以便早暴露。
		panic("assets: fs.Sub(files) failed: " + err.Error())
	}
	return sub
}
