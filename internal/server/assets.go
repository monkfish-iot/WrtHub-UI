package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/assets"
)

// registerAssetsRoutes 注册 /assets/* 静态资源路由（外部目录优先，回退 embed）。
//
// 见开发技术文档 §8：运行时优先读 cfg.Assets.ExternalDir（默认 ./assets），
// 文件不存在则回退到 internal/assets 内置的 go:embed 资源。
// 资源路径在两处保持一致（css/...、js/...、webfonts/...），无需区分来源。
// /assets/* 不受 RequireAuth 保护：登录页也需要 Bootstrap/htmx。
//
// 缓存：响应带 Cache-Control(1h) + ETag（size-modtime）。embed 文件 ModTime 为
// 零值，http.ServeContent 不会发 Last-Modified，导致浏览器每次跳页全量重新
// 下载 CSS/字体（公网下延迟数秒），故手动实现 ETag/304 协商缓存。
func registerAssetsRoutes(r *gin.Engine, app *App) {
	embedFS := assets.FS()
	externalDir := app.Cfg.Assets.ExternalDir

	r.GET("/assets/*path", func(c *gin.Context) {
		// c.Param("path") 形如 "/css/bootstrap.min.css"，含前导 "/"。
		rel := strings.TrimPrefix(c.Param("path"), "/")
		if rel == "" || strings.Contains(rel, "..") {
			http.NotFound(c.Writer, c.Request)
			return
		}

		// 1. 优先外部目录（仅普通文件；目录请求交给 embed 兜底，避免列出外部目录）
		if externalDir != "" {
			full := filepath.Join(externalDir, filepath.FromSlash(rel))
			if info, err := os.Stat(full); err == nil && !info.IsDir() {
				if serveAssetNotModified(c, info.Size(), info.ModTime().UnixNano()) {
					return
				}
				http.ServeFile(c.Writer, c.Request, full)
				return
			}
		}

		// 2. 回退 embed：assets.FS() 已 sub 到 files/ 内容层，路径形如 "css/x.css"。
		f, err := embedFS.Open(rel)
		if err != nil {
			http.NotFound(c.Writer, c.Request)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil || stat.IsDir() {
			http.NotFound(c.Writer, c.Request)
			return
		}
		if serveAssetNotModified(c, stat.Size(), stat.ModTime().UnixNano()) {
			return
		}
		// embed 文件实现 io.ReadSeeker；ServeContent 处理 Content-Type / Last-Modified / Range。
		rs, ok := f.(io.ReadSeeker)
		if !ok {
			http.Error(c.Writer, "internal: embedded file not seekable", http.StatusInternalServerError)
			return
		}
		http.ServeContent(c.Writer, c.Request, stat.Name(), stat.ModTime(), rs)
	})
}

// serveAssetNotModified 设置缓存头并处理 If-None-Match 协商；命中则返回 true（已写 304）。
func serveAssetNotModified(c *gin.Context, size int64, modNano int64) bool {
	etag := fmt.Sprintf(`"%x-%x"`, size, modNano)
	h := c.Writer.Header()
	h.Set("Cache-Control", "public, max-age=3600")
	h.Set("ETag", etag)
	if c.GetHeader("If-None-Match") == etag {
		c.Status(http.StatusNotModified)
		return true
	}
	return false
}
