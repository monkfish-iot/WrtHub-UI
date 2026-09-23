// Package server 负责装配各业务模块为 gin 路由树，是 main 与各 internal 包之间的胶水层。
//
// App 持有运行期所有单例依赖（DB / 认证 / frps_helper / LuCI RPC / 凭证 provider），
// 供 router.Mount 注册路由时按需取用。
package server

import (
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"wrthub-ui/internal/auth"
	"wrthub-ui/internal/config"
	"wrthub-ui/internal/frpsclient"
	"wrthub-ui/internal/lucirpc"
	"wrthub-ui/internal/render"
	"wrthub-ui/internal/service"
	"wrthub-ui/internal/store"
	"wrthub-ui/internal/ubusrpc"
)

// App 持有运行期单例依赖。
// RPC（LuCI RPC）是默认访问通道，Ubus（UBUS JSON-RPC）作为 RPC 失败时的兜底，
// 两者复用同一 CredentialProvider 与并发限流配置。
type App struct {
	Cfg      *config.Config
	Log      *slog.Logger
	DB       *gorm.DB
	Auth     auth.Authenticator
	Frps     *frpsclient.Client
	RPC      *lucirpc.Client
	Ubus     *ubusrpc.Client
	CredProv *service.CredentialProvider
	Render   *render.Renderer
	Wireless *service.Wireless
}

// New 装配所有依赖：打开 DB、初始化管理员、构造 frps_helper / 凭证 / LuCI RPC client、选择认证 provider。
func New(cfg *config.Config, log *slog.Logger) (*App, error) {
	db, err := store.Open(cfg.Storage.Driver, cfg.Storage.SqlitePath, cfg.Storage.PostgresDSN)
	if err != nil {
		return nil, err
	}
	if err := store.SeedAdmin(db, cfg.Auth.InitUsername, cfg.Auth.InitPassword, log); err != nil {
		return nil, err
	}
	if err := store.SeedRBAC(db); err != nil {
		return nil, fmt.Errorf("seed rbac: %w", err)
	}

	frpc := frpsclient.New(frpsclient.Options{
		BaseURL:     cfg.FrpsHelper.BaseURL,
		TLSInsecure: cfg.FrpsHelper.TLSInsecure,
		Timeout:     cfg.FrpsHelper.RequestTimeout,
	})
	credProv := service.NewCredentialProvider(frpc, 0, cfg.DeviceRpc.TokenCacheTTL)
	rpcClient := lucirpc.New(lucirpc.Options{
		Scheme:         cfg.DeviceRpc.Scheme,
		DomainSuffix:   cfg.DeviceRpc.DomainSuffix,
		Port:           cfg.DeviceRpc.Port,
		AuthPath:       cfg.DeviceRpc.AuthPath,
		RpcPathPrefix:  cfg.DeviceRpc.RpcPathPrefix,
		TokenCacheTTL:  cfg.DeviceRpc.TokenCacheTTL,
		Timeout:        cfg.DeviceRpc.Timeout,
		MaxConcurrency: cfg.DeviceRpc.MaxConcurrency,
	}, credProv, log)
	ubusClient := ubusrpc.New(ubusrpc.Options{
		Scheme:         cfg.DeviceRpc.Scheme,
		DomainSuffix:   cfg.DeviceRpc.DomainSuffix,
		Port:           cfg.DeviceRpc.Port,
		UbusPath:       cfg.DeviceRpc.UbusPath,
		Timeout:        cfg.DeviceRpc.Timeout,
		MaxConcurrency: cfg.DeviceRpc.MaxConcurrency,
	}, rpcClient, log)

	authProv := auth.NewLocalAuthenticator(db, log)
	if cfg.Auth.Mode != "local" {
		// CASDOOR 等其他 provider 待后续实现；当前仅 local 可用，降级并告警。
		log.Warn("auth mode is not 'local'; only local provider is implemented, falling back to local", "mode", cfg.Auth.Mode)
		authProv = auth.NewLocalAuthenticator(db, log)
	}

	renderer, err := render.NewRenderer(cfg.Assets.TemplatesDir)
	if err != nil {
		return nil, fmt.Errorf("init renderer: %w", err)
	}

	return &App{
		Cfg:      cfg,
		Log:      log,
		DB:       db,
		Auth:     authProv,
		Frps:     frpc,
		RPC:      rpcClient,
		Ubus:     ubusClient,
		CredProv: credProv,
		Render:   renderer,
		Wireless: service.NewWireless(ubusClient, log),
	}, nil
}
