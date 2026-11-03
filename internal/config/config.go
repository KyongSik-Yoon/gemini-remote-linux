package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxBootstrapFileSize = 32768 // 0x8000 bytes (macOS Gemini.app 상한과 동일)
	DefaultOSVersion     = "15.4.0"
	DefaultOSBuild       = "24E248"
	DefaultAppVersion    = "1.2026.0820001"
)

// MCPServerConfig는 외부 또는 내장 MCP 서버 연결 설정을 담는다.
type MCPServerConfig struct {
	ServerID    string            `json:"server_id"`
	DisplayName string            `json:"display_name"`
	Instruction string            `json:"instruction,omitempty"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Enabled     bool              `json:"enabled"`
}

// Config는 gemini-remote-linux 데몬의 영구 설정을 담는다.
type Config struct {
	DeviceID         string            `json:"device_id"`
	DeviceName       string            `json:"device_name"`
	OSType           string            `json:"os_type"`
	OSVersion        string            `json:"os_version"`
	OSBuild          string            `json:"os_build"`
	AppVersion       string            `json:"app_version"`
	Locale           string            `json:"locale"`
	TimeZone         string            `json:"time_zone"`
	ConnectedFolders  []string          `json:"connected_folders"`
	MCPServers        []MCPServerConfig `json:"mcp_servers"`
	ObsidianVaultDir  string            `json:"obsidian_vault_dir,omitempty"`
	OAuthClientID     string            `json:"oauth_client_id,omitempty"`
	OAuthClientSecret string            `json:"oauth_client_secret,omitempty"`
}

// ConfigDir는 ~/.config/gemini-remote 경로를 반환한다.
func ConfigDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "gemini-remote")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ConfigPath는 설정 파일 경로를 반환한다.
func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// TokenPath는 OAuth 토큰 저장 경로를 반환한다.
func TokenPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "token.json"), nil
}

// LoadOrInit은 설정 파일을 읽거나 기본값으로 초기화해 저장한다.
func LoadOrInit() (*Config, error) {
	p, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(p); err == nil {
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err == nil {
			cfg.applyDefaults()
			return &cfg, nil
		}
	}
	cfg := &Config{}
	cfg.applyDefaults()
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if strings.TrimSpace(c.DeviceID) == "" {
		c.DeviceID = newUUIDv4()
	}
	c.DeviceID = strings.ToUpper(strings.TrimSpace(c.DeviceID))
	if strings.TrimSpace(c.DeviceName) == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "Linux-Mac-Bridge"
		}
		c.DeviceName = host
	}
	// macOS Gemini.app 프로토콜 호환을 위해 기본값은 "macOS"로 유지한다.
	if strings.TrimSpace(c.OSType) == "" {
		c.OSType = "macOS"
	}
	if strings.TrimSpace(c.OSVersion) == "" {
		c.OSVersion = DefaultOSVersion
	}
	if strings.TrimSpace(c.OSBuild) == "" {
		c.OSBuild = DefaultOSBuild
	}
	if strings.TrimSpace(c.AppVersion) == "" {
		c.AppVersion = DefaultAppVersion
	}
	if strings.TrimSpace(c.Locale) == "" {
		c.Locale = "ko-KR"
	}
	if strings.TrimSpace(c.TimeZone) == "" {
		c.TimeZone = detectTimeZone()
	}
	if len(c.ConnectedFolders) == 0 {
		home, _ := os.UserHomeDir()
		if home != "" {
			gitRepo := filepath.Join(home, "dev", "git-repo")
			if st, err := os.Stat(gitRepo); err == nil && st.IsDir() {
				c.ConnectedFolders = []string{gitRepo}
			} else {
				c.ConnectedFolders = []string{home}
			}
		}
	}
	if len(c.MCPServers) == 0 {
		c.MCPServers = []MCPServerConfig{
			{
				ServerID:    "git",
				DisplayName: "Git",
				Instruction: "Local Git repository inspection and management tools",
				Enabled:     true,
			},
			{
				ServerID:    "obsidian",
				DisplayName: "Obsidian",
				Instruction: "Local Markdown and Obsidian note search and editing tools",
				Enabled:     true,
			},
		}
	}
}

// Save는 설정을 디스크에 원자적으로 기록한다.
func (c *Config) Save() error {
	p, err := ConfigPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// CollectBootstrapFiles는 연결된 폴더 및 홈 디렉토리의 AGENTS.md / CLAUDE.md / GEMINI.md 파일을 수집한다.
// macOS Gemini.app과 동일하게 심볼릭 링크는 건너뛰고 32,768바이트까지만 읽는다.
func (c *Config) CollectBootstrapFiles() map[string][]byte {
	out := make(map[string][]byte)
	home, _ := os.UserHomeDir()

	candidates := []struct {
		key  string
		path string
	}{}

	if home != "" {
		candidates = append(candidates,
			struct{ key, path string }{"~/.gemini/AGENTS.md", filepath.Join(home, ".gemini", "AGENTS.md")},
			struct{ key, path string }{"~/.gemini/GEMINI.md", filepath.Join(home, ".gemini", "GEMINI.md")},
			struct{ key, path string }{"~/AGENTS.md", filepath.Join(home, "AGENTS.md")},
		)
	}
	for _, folder := range c.ConnectedFolders {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"} {
			full := filepath.Join(folder, name)
			candidates = append(candidates, struct{ key, path string }{full, full})
		}
	}

	for _, cand := range candidates {
		fi, err := os.Lstat(cand.path)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(cand.path)
		if err != nil || len(data) == 0 {
			continue
		}
		if len(data) > MaxBootstrapFileSize {
			data = data[:MaxBootstrapFileSize]
		}
		out[cand.key] = data
	}
	const remoteFileNote = "\n\n## Remote File & Image Sharing Rule\nWhen the user asks to view, show, or share a local image, screenshot, or file in the Gemini app, ALWAYS call `view_file` (or `upload_to_drive`) and include the returned Google Drive `web_view_link` as a clickable Markdown link in your response. NEVER use `run_command` with `base64` to encode images.\n"
	if existing, ok := out["~/AGENTS.md"]; ok && len(existing)+len(remoteFileNote) <= MaxBootstrapFileSize {
		out["~/AGENTS.md"] = append(existing, []byte(remoteFileNote)...)
	}
	return out
}

func detectTimeZone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if idx := strings.Index(target, "zoneinfo/"); idx != -1 {
			return target[idx+len("zoneinfo/"):]
		}
	}
	return "Asia/Seoul"
}

func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
