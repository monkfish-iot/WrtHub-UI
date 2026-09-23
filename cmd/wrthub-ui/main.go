package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"wrthub-ui/internal/config"
	"wrthub-ui/internal/logger"
	"wrthub-ui/internal/server"
)

func main() {
	cfgDir := flag.String("d", "", "配置目录（含 config.yaml 与 data/）；缺省依次尝试 "+config.DefaultDir+"、当前目录")
	flag.Parse()

	cfgPath, err := config.ResolvePath(*cfgDir)
	if err != nil {
		slog.Error("resolve config failed", "err", err)
		os.Exit(1)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("load config failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}
	log := logger.Init(cfg.App.LogLevel)
	log.Info("config loaded",
		"config", cfgPath,
		"server", cfg.Server.Addr,
		"frps_helper", cfg.FrpsHelper.BaseURL,
		"device_rpc", cfg.DeviceRpc.DomainSuffix,
		"storage", cfg.Storage.Driver,
		"sqlite", cfg.Storage.SqlitePath,
	)

	app, err := server.New(cfg, log)
	if err != nil {
		log.Error("init app failed", "err", err)
		os.Exit(1)
	}

	r := gin.Default()
	server.Mount(r, app)

	srv := &http.Server{Addr: cfg.Server.Addr, Handler: r}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()
	log.Info("server starting", "addr", cfg.Server.Addr)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown error", "err", err)
	}
}
