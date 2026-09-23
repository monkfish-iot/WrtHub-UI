package logger

import (
	"log/slog"
	"os"
	"strings"
)

// Init 初始化全局 slog 日志器，返回实例并设为默认。
func Init(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}
