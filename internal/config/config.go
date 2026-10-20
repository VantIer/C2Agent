// Package config loads and validates the C2Agent configuration.
//
// The configuration is partitioned into llm / web / native / shell / policy /
// transfer sections. The two controlled-end families (native protocol agents
// and reverse-shell bots) have independent, coexisting settings.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultSystemPrompt = `You are an AI assistant that helps the user execute tasks.
CURRENT OPERATING SYSTEM: {system_name}

You accomplish tasks exclusively through the provided tools. Rules:
- Prefer absolute paths; the shell does not keep state between commands.
- Read-only tools (get_cwd, list_dir, read_file) are safe; higher-risk tools may require user authorization.
- For long-running tasks, start them detached (e.g. nohup ... & / start "" /b ...) so the tool returns promptly.
`

// Config is the root configuration object.
type Config struct {
	Mode       string     `json:"mode"` // startup mode: cli | web
	LLM        LLM        `json:"llm"`
	Web        Web        `json:"web"`
	Native     Native     `json:"native"`
	Shell      Shell      `json:"shell"`
	FileServer FileServer `json:"fileserver"`
	Policy     Policy     `json:"policy"`
	Transfer   Transfer   `json:"transfer"`
}

// LLM holds the OpenAI-compatible endpoint settings.
type LLM struct {
	APIBase      string  `json:"api_base"`
	APIKey       string  `json:"api_key"`
	Model        string  `json:"model"`
	Temperature  float64 `json:"temperature"`
	Stream       bool    `json:"stream"`
	SystemPrompt string  `json:"system_prompt"`
}

// Web holds the browser panel listener settings.
type Web struct {
	ListenHost string `json:"listen_host"`
	ListenPort int    `json:"listen_port"`
}

// Native holds the binary-protocol agent listener settings.
type Native struct {
	Enabled             bool   `json:"enabled"`
	ListenHost          string `json:"listen_host"`
	ListenPort          int    `json:"listen_port"`
	AuthToken           string `json:"auth_token"`
	HeartbeatTimeoutSec int    `json:"heartbeat_timeout_sec"`
}

// Shell holds the reverse-shell listener settings.
type Shell struct {
	Enabled    bool   `json:"enabled"`
	ListenHost string `json:"listen_host"`
	ListenPort int    `json:"listen_port"`
	BotPrefix  string `json:"bot_prefix"`
}

// FileServer holds the optional static file server settings. It is completely
// independent of the C2 listeners and has no authentication.
type FileServer struct {
	Enabled    bool   `json:"enable"`
	ListenHost string `json:"listen_host"`
	ListenPort int    `json:"listen_port"`
	Path       string `json:"path"`
}

// Policy holds authorization / scheduling limits.
type Policy struct {
	AuthMode            int    `json:"auth_mode"`              // 0=N-Auto 1=H-Auto 2=F-Auto
	RoundLimit          int    `json:"round_limit"`            // max LLM rounds per turn
	CmdTimeout          int    `json:"cmd_timeout"`            // execution timeout (seconds)
	TimeoutAction       string `json:"timeout_action"`         // "disconnect" | "fail"
	MaxSessionsPerAgent int    `json:"max_sessions_per_agent"` // sessions per agent cap
	QueueCapacity       int    `json:"queue_capacity"`         // per-agent job queue depth
}

// Transfer holds download / upload staging directories.
type Transfer struct {
	DlTempDir string `json:"dl_temp_dir"`
	UlTempDir string `json:"ul_temp_dir"`
}

// Default returns a Config populated with built-in defaults.
func Default() *Config {
	return &Config{
		Mode: "cli",
		LLM: LLM{
			APIBase:      "http://localhost:11434/v1",
			APIKey:       "deepseek",
			Model:        "deepseek",
			Temperature:  0.7,
			Stream:       true,
			SystemPrompt: defaultSystemPrompt,
		},
		Web: Web{ListenHost: "127.0.0.1", ListenPort: 8880},
		Native: Native{
			// Native agents require an explicit auth token; disabled by default
			// so the binary can start with no config (shell listener only).
			Enabled:             false,
			ListenHost:          "0.0.0.0",
			ListenPort:          8881,
			HeartbeatTimeoutSec: 60,
		},
		Shell: Shell{
			Enabled:    true,
			ListenHost: "0.0.0.0",
			ListenPort: 8882,
			BotPrefix:  "BOT-",
		},
		FileServer: FileServer{
			Enabled:    false,
			ListenHost: "0.0.0.0",
			ListenPort: 8883,
		},
		Policy: Policy{
			AuthMode:            0,
			RoundLimit:          20,
			CmdTimeout:          60,
			TimeoutAction:       "disconnect",
			MaxSessionsPerAgent: 8,
			QueueCapacity:       256,
		},
	}
}

// Load reads a JSON config file, applying defaults for missing fields, then
// validates the result. A nonexistent path yields defaults (with a validation
// pass) so the binary can still start for local experiments.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// normalize repairs / validates fields and returns an error on fatal problems.
func (c *Config) normalize() error {
	if c.Mode == "" {
		c.Mode = "cli"
	}
	switch c.Mode {
	case "cli", "web":
	default:
		c.Mode = "cli"
	}
	if c.Native.ListenHost == "" {
		c.Native.ListenHost = "0.0.0.0"
	}
	if c.Shell.ListenHost == "" {
		c.Shell.ListenHost = "0.0.0.0"
	}
	if c.Shell.BotPrefix == "" {
		c.Shell.BotPrefix = "BOT-"
	}
	if c.FileServer.ListenHost == "" {
		c.FileServer.ListenHost = "0.0.0.0"
	}
	if c.FileServer.ListenPort == 0 {
		c.FileServer.ListenPort = 8883
	}
	if c.Web.ListenHost == "" {
		c.Web.ListenHost = "127.0.0.1"
	}
	if c.Native.HeartbeatTimeoutSec <= 0 {
		c.Native.HeartbeatTimeoutSec = 60
	}
	if c.Policy.RoundLimit <= 0 {
		c.Policy.RoundLimit = 20
	}
	if c.Policy.CmdTimeout <= 0 {
		c.Policy.CmdTimeout = 60
	}
	if c.Policy.MaxSessionsPerAgent <= 0 {
		c.Policy.MaxSessionsPerAgent = 8
	}
	if c.Policy.QueueCapacity <= 0 {
		c.Policy.QueueCapacity = 256
	}
	if c.LLM.SystemPrompt == "" {
		c.LLM.SystemPrompt = defaultSystemPrompt
	}
	if c.Policy.AuthMode < 0 || c.Policy.AuthMode > 2 {
		c.Policy.AuthMode = 0
	}
	switch c.Policy.TimeoutAction {
	case "disconnect", "fail":
	default:
		c.Policy.TimeoutAction = "disconnect"
	}

	if c.Native.Enabled && strings.TrimSpace(c.Native.AuthToken) == "" {
		return fmt.Errorf("native.enabled=true requires a non-empty native.auth_token")
	}
	if c.Native.Enabled && (c.Native.ListenPort < 1 || c.Native.ListenPort > 65535) {
		return fmt.Errorf("native.listen_port out of range: %d", c.Native.ListenPort)
	}
	if c.Shell.Enabled && (c.Shell.ListenPort < 1 || c.Shell.ListenPort > 65535) {
		return fmt.Errorf("shell.listen_port out of range: %d", c.Shell.ListenPort)
	}
	if c.Native.Enabled && c.Policy.CmdTimeout < c.Native.HeartbeatTimeoutSec {
		return fmt.Errorf("policy.cmd_timeout (%ds) must be >= native.heartbeat_timeout_sec (%ds)",
			c.Policy.CmdTimeout, c.Native.HeartbeatTimeoutSec)
	}
	if c.Native.Enabled && c.Shell.Enabled && c.Native.ListenPort == c.Shell.ListenPort &&
		hostsConflict(c.Native.ListenHost, c.Shell.ListenHost) {
		return fmt.Errorf("native and shell listeners share %s:%d", c.Native.ListenHost, c.Native.ListenPort)
	}
	if c.Web.ListenPort == 0 {
		c.Web.ListenPort = 8880
	}
	if c.Web.ListenPort < 1 || c.Web.ListenPort > 65535 {
		return fmt.Errorf("web.listen_port out of range: %d", c.Web.ListenPort)
	}
	if c.Native.Enabled && c.Web.ListenPort == c.Native.ListenPort &&
		hostsConflict(c.Web.ListenHost, c.Native.ListenHost) {
		return fmt.Errorf("web and native listeners share %s:%d", c.Web.ListenHost, c.Web.ListenPort)
	}
	if c.Shell.Enabled && c.Web.ListenPort == c.Shell.ListenPort &&
		hostsConflict(c.Web.ListenHost, c.Shell.ListenHost) {
		return fmt.Errorf("web and shell listeners share %s:%d", c.Web.ListenHost, c.Web.ListenPort)
	}
	return nil
}

// PrepareFileServer validates the optional file server settings and returns
// its resolved root. An error means the file server must stay disabled; the
// rest of the program keeps running.
func (c *Config) PrepareFileServer() (string, error) {
	if !c.FileServer.Enabled {
		return "", nil
	}
	if c.FileServer.ListenPort < 1 || c.FileServer.ListenPort > 65535 {
		return "", fmt.Errorf("fileserver.listen_port out of range: %d", c.FileServer.ListenPort)
	}
	if c.Web.ListenPort == c.FileServer.ListenPort &&
		hostsConflict(c.Web.ListenHost, c.FileServer.ListenHost) {
		return "", fmt.Errorf("fileserver and web listeners share %s:%d",
			c.FileServer.ListenHost, c.FileServer.ListenPort)
	}
	if c.Native.Enabled && c.Native.ListenPort == c.FileServer.ListenPort &&
		hostsConflict(c.Native.ListenHost, c.FileServer.ListenHost) {
		return "", fmt.Errorf("fileserver and native listeners share %s:%d",
			c.FileServer.ListenHost, c.FileServer.ListenPort)
	}
	if c.Shell.Enabled && c.Shell.ListenPort == c.FileServer.ListenPort &&
		hostsConflict(c.Shell.ListenHost, c.FileServer.ListenHost) {
		return "", fmt.Errorf("fileserver and shell listeners share %s:%d",
			c.FileServer.ListenHost, c.FileServer.ListenPort)
	}
	return c.ResolveFileServerRoot()
}

// ResolveFileServerRoot resolves the served directory. An empty path means
// <executable dir>/fileserver, which is created if needed; an explicit path
// must already exist and be a directory.
func (c *Config) ResolveFileServerRoot() (string, error) {
	p := strings.TrimSpace(c.FileServer.Path)
	if p == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("fileserver: resolve executable path: %w", err)
		}
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		p = filepath.Join(filepath.Dir(exe), "fileserver")
		if err := os.MkdirAll(p, 0o755); err != nil {
			return "", fmt.Errorf("fileserver: create default directory %s: %w", p, err)
		}
		return p, nil
	}

	p = expandUser(p)
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("fileserver: resolve path %q: %w", p, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("fileserver: path does not exist: %s", abs)
		}
		return "", fmt.Errorf("fileserver: stat path %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("fileserver: path is not a directory: %s", abs)
	}
	return abs, nil
}

// DlDir returns the download staging directory, creating it if needed.
// Empty dl_temp_dir means <cwd>/downloads.
func (c *Config) DlDir() (string, error) {
	dir := strings.TrimSpace(c.Transfer.DlTempDir)
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cwd, "downloads")
	} else {
		dir = expandUser(dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// UlDir returns the upload staging directory, creating it if needed.
// Empty ul_temp_dir means the system temp directory.
func (c *Config) UlDir() (string, error) {
	dir := strings.TrimSpace(c.Transfer.UlTempDir)
	if dir == "" {
		dir = os.TempDir()
	} else {
		dir = expandUser(dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// hostsConflict reports whether two listen hosts would bind the same address,
// treating a wildcard host as conflicting with anything on the same port.
func hostsConflict(a, b string) bool {
	if a == b {
		return true
	}
	wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" }
	return wild(a) || wild(b)
}

func expandUser(p string) string {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				return home
			}
			if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
				return filepath.Join(home, p[2:])
			}
		}
	}
	return p
}
