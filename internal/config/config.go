package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// 配置目录约定：部署时 config.yaml 与 data/ 都放在 DefaultDir；
// 默认目录不存在（如开发机）时回退到当前工作目录，也可用 -d 显式指定。
const (
	// DefaultDir 是默认配置目录，内含 config.yaml 与 data/。
	DefaultDir = "/usr/local/etc/wrthub-ui"
	// FileName 是配置目录下的配置文件名。
	FileName = "config.yaml"
)

// Config 是 WrtHub-UI 的运行时配置（对应 config.yaml）。
type Config struct {
	App        AppConfig        `yaml:"app"`
	Server     ServerConfig     `yaml:"server"`
	FrpsHelper FrpsHelperConfig `yaml:"frps_helper"`
	DeviceRpc  DeviceRpcConfig  `yaml:"device_rpc"`
	Auth       AuthConfig       `yaml:"auth"`
	Storage    StorageConfig    `yaml:"storage"`
	Assets     AssetsConfig     `yaml:"assets"`
}

type AppConfig struct {
	LogLevel string `yaml:"log_level"`
}

type ServerConfig struct {
	Addr          string `yaml:"addr"`
	SessionSecret string `yaml:"session_secret"`
}

type FrpsHelperConfig struct {
	BaseURL         string        `yaml:"base_url"`
	TLSInsecure     bool          `yaml:"tls_insecure"`
	CredentialQuery bool          `yaml:"credential_query"`
	RequestTimeout  time.Duration `yaml:"request_timeout"`
}

type DeviceRpcConfig struct {
	Scheme         string        `yaml:"scheme"`
	DomainSuffix   string        `yaml:"domain_suffix"`
	Port           int           `yaml:"port"`
	AuthPath       string        `yaml:"auth_path"`
	RpcPathPrefix  string        `yaml:"rpc_path_prefix"`
	UbusPath       string        `yaml:"ubus_path"` // UBUS JSON-RPC 端点路径，默认 /ubus；lucirpc 失败时兜底
	TokenCacheTTL  time.Duration `yaml:"token_cache_ttl"`
	Timeout        time.Duration `yaml:"timeout"`
	MaxConcurrency int           `yaml:"max_concurrency"`
}

type AuthConfig struct {
	Mode         string `yaml:"mode"`          // local | casdoor
	InitUsername string `yaml:"init_username"` // 首次启动自动创建的管理员用户名
	InitPassword string `yaml:"init_password"` // 首次启动自动创建的管理员初始密码；为空则用默认值并告警
}

type StorageConfig struct {
	Driver      string `yaml:"driver"`
	SqlitePath  string `yaml:"sqlite_path"`
	PostgresDSN string `yaml:"postgres_dsn"`
}

type AssetsConfig struct {
	ExternalDir  string `yaml:"external_dir"`
	TemplatesDir string `yaml:"templates_dir"`
}

// ResolvePath 决定实际使用的配置文件路径，优先级由高到低：
//  1. flagDir：命令行 -d 指定的配置目录（该目录下必须有 config.yaml）；
//  2. 环境变量 WRTHUB_CONFIG：完整配置文件路径；
//  3. DefaultDir 下存在 config.yaml；
//  4. 当前工作目录下的 config.yaml。
func ResolvePath(flagDir string) (string, error) {
	if flagDir != "" {
		p, err := filepath.Abs(filepath.Join(flagDir, FileName))
		if err != nil {
			return "", fmt.Errorf("resolve config dir %q: %w", flagDir, err)
		}
		if info, err := os.Stat(p); err != nil || info.IsDir() {
			return "", fmt.Errorf("config file not found: %s", p)
		}
		return p, nil
	}
	if v := os.Getenv("WRTHUB_CONFIG"); v != "" {
		return v, nil
	}
	p := filepath.Join(DefaultDir, FileName)
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return p, nil
	}
	return FileName, nil
}

// Load 读取并解析 path 指向的 YAML 配置文件，应用默认值，
// 并把配置中的相对路径锚定到配置文件所在目录（见 resolvePaths）。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.applyDefaults()
	c.resolvePaths(filepath.Dir(path))
	return &c, nil
}

// resolvePaths 把配置里的相对路径锚定到配置目录，使 data/、assets/、templates/
// 始终相对 config.yaml 所在目录解析，与进程启动时的 cwd 无关；绝对路径原样保留。
func (c *Config) resolvePaths(dir string) {
	c.Storage.SqlitePath = anchor(dir, c.Storage.SqlitePath)
	c.Assets.ExternalDir = anchor(dir, c.Assets.ExternalDir)
	c.Assets.TemplatesDir = anchor(dir, c.Assets.TemplatesDir)
}

func anchor(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

func (c *Config) applyDefaults() {
	if c.App.LogLevel == "" {
		c.App.LogLevel = "info"
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.FrpsHelper.RequestTimeout == 0 {
		c.FrpsHelper.RequestTimeout = 10 * time.Second
	}
	if c.DeviceRpc.Scheme == "" {
		c.DeviceRpc.Scheme = "http"
	}
	if c.DeviceRpc.DomainSuffix == "" {
		c.DeviceRpc.DomainSuffix = "frpclient.local"
	}
	if c.DeviceRpc.Port == 0 {
		c.DeviceRpc.Port = 20001
	}
	if c.DeviceRpc.AuthPath == "" {
		c.DeviceRpc.AuthPath = "/auth"
	}
	if c.DeviceRpc.RpcPathPrefix == "" {
		c.DeviceRpc.RpcPathPrefix = "/cgi-bin/luci/rpc"
	}
	if c.DeviceRpc.UbusPath == "" {
		c.DeviceRpc.UbusPath = "/ubus"
	}
	if c.DeviceRpc.TokenCacheTTL == 0 {
		c.DeviceRpc.TokenCacheTTL = 5 * time.Minute
	}
	if c.DeviceRpc.Timeout == 0 {
		c.DeviceRpc.Timeout = 10 * time.Second
	}
	if c.DeviceRpc.MaxConcurrency == 0 {
		c.DeviceRpc.MaxConcurrency = 4
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = "local"
	}
	if c.Auth.InitUsername == "" {
		c.Auth.InitUsername = "admin"
	}
	if c.Auth.InitPassword == "" {
		c.Auth.InitPassword = "admin@123"
	}
	if c.Storage.Driver == "" {
		c.Storage.Driver = "sqlite"
	}
	if c.Storage.SqlitePath == "" {
		c.Storage.SqlitePath = "data/wrthub.db"
	}
	if c.Assets.ExternalDir == "" {
		c.Assets.ExternalDir = "./assets"
	}
	if c.Assets.TemplatesDir == "" {
		c.Assets.TemplatesDir = "./templates"
	}
}

// DeviceHost 由 session_id 拼装设备访问地址：http://{session_id}.frpclient.local:20001
func (c *Config) DeviceHost(sessionID string) string {
	return fmt.Sprintf("%s://%s.%s:%d", c.DeviceRpc.Scheme, sessionID, c.DeviceRpc.DomainSuffix, c.DeviceRpc.Port)
}
