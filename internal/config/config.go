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
	LLM      LLM      `json:"llm"`
	Web      Web      `json:"web"`
	Native   Native   `json:"native"`
	Shell    Shell    `json:"shell"`
	Policy   Policy   `json:"policy"`
	Transfer Transfer `json:"transfer"`
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
		LLM: LLM{
			APIBase:      "http://localhost/v1",
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
		} else {
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
	if c.Native.ListenHost == "" {
		c.Native.ListenHost = "0.0.0.0"
	}
	if c.Shell.ListenHost == "" {
		c.Shell.ListenHost = "0.0.0.0"
	}
	if c.Shell.BotPrefix == "" {
		c.Shell.BotPrefix = "BOT-"
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
	if c.Native.Enabled && c.Shell.Enabled && c.Native.ListenPort == c.Shell.ListenPort &&
		c.Native.ListenHost == c.Shell.ListenHost {
		return fmt.Errorf("native and shell listeners share %s:%d", c.Native.ListenHost, c.Native.ListenPort)
	}
	if c.Web.ListenPort == 0 {
		c.Web.ListenPort = 8880
	}
	return nil
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
