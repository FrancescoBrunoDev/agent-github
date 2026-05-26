package config

import (
	"encoding/json"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds all service configuration.
type Config struct {
	ListenAddr  string `yaml:"listen_addr"`
	WebhookPath string `yaml:"webhook_path"`
	MaxWorkers  int    `yaml:"max_workers"`
	MediaDir    string `yaml:"media_dir"`
	PreviewDir  string `yaml:"preview_dir"`
	BaseURL     string `yaml:"base_url"`

	GitHub GitHubConfig           `yaml:"github"`
	Queue  QueueConfig            `yaml:"queue"`
	Pi     PiConfig               `yaml:"pi"`
	Repos  map[string]RepoConfig  `yaml:"repos"` // key = owner/repo
}

// GitHubConfig holds GitHub App credentials.
type GitHubConfig struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyFile string `yaml:"private_key_file"`
	WebhookSecret  string `yaml:"webhook_secret"`
	// BotUser is the GitHub username of a machine/bot user that the agent impersonates
	// for actions the App cannot perform (e.g., issue assignment).
	BotUser string `yaml:"bot_user"`
	// BotToken is a personal access token (PAT) for the bot user.
	BotToken string `yaml:"bot_token"`
}

// QueueConfig holds queue settings.
type QueueConfig struct {
	Type string `yaml:"type"` // "memory", "redis" (future)
}

// PiConfig holds pi agent configuration injected into the worker container.
type PiConfig struct {
	// Settings becomes ~/.pi/agent/settings.json
	Settings *PiSettings `yaml:"settings"`
	// Models becomes ~/.pi/agent/models.json (raw JSON)
	Models string `yaml:"models"`
	// Auth becomes ~/.pi/agent/auth.json (raw JSON)
	Auth string `yaml:"auth"`
}

// RepoConfig holds per-repo settings that apply only when processing jobs
// from a specific repository.
type RepoConfig struct {
	// Env defines extra environment variables injected into the worker
	// container for this repo (e.g., API keys, feature flags).
	Env map[string]string `yaml:"env"`

	// Nixpacks overrides for containerized preview builds.
	Nixpacks *NixpacksConfig `yaml:"nixpacks"`

	// Preview controls how project previews are built and served.
	// If omitted, defaults to Nixpacks auto-detection.
	Preview *PreviewConfig `yaml:"preview"`
}

// NixpacksConfig overrides auto-detection when building preview containers.
type NixpacksConfig struct {
	InstallCmd string `yaml:"install_cmd"`
	BuildCmd   string `yaml:"build_cmd"`
	StartCmd   string `yaml:"start_cmd"`
}

// PreviewConfig controls how the preview container is built.
type PreviewConfig struct {
	// Type is the build method: "nixpacks" (default), "dockerfile", or "docker-compose".
	Type string `yaml:"type"`

	// DockerfilePath is the path to the Dockerfile (default: "Dockerfile").
	DockerfilePath string `yaml:"dockerfile_path"`

	// DockerComposeFile is the path to the docker-compose file (default: "docker-compose.yml").
	DockerComposeFile string `yaml:"docker_compose_file"`

	// DockerComposeService is the service name to expose (default: first service with ports).
	DockerComposeService string `yaml:"docker_compose_service"`

	// BuildArgs are passed to docker build --build-arg.
	BuildArgs map[string]string `yaml:"build_args"`

	// Port is the internal container port to expose (default: auto-detect 3000/8080/80).
	Port int `yaml:"port"`
}

// PiSettings holds the most common pi agent settings.
type PiSettings struct {
	DefaultProvider      string   `yaml:"default_provider" json:"defaultProvider,omitempty"`
	DefaultModel         string   `yaml:"default_model" json:"defaultModel,omitempty"`
	DefaultThinkingLevel string   `yaml:"default_thinking_level" json:"defaultThinkingLevel,omitempty"`
	Theme                string   `yaml:"theme" json:"theme,omitempty"`
	Skills               []string `yaml:"skills" json:"skills,omitempty"`
}

// SettingsJSON serializes pi settings to JSON.
func (s *PiSettings) SettingsJSON() string {
	if s == nil {
		return "{}"
	}
	data, _ := json.Marshal(s)
	return string(data)
}

// Defaults returns a Config with sensible defaults.
func Defaults() Config {
	return Config{
		ListenAddr:  ":9423",
		WebhookPath: "/webhook",
		MaxWorkers:  5,
		MediaDir:    "./media",
		PreviewDir:  "./preview",
		BaseURL:     "http://localhost:9423",
		Queue: QueueConfig{
			Type: "memory",
		},
		Pi: PiConfig{
			Settings: &PiSettings{
				DefaultProvider:      "anthropic",
				DefaultModel:         "claude-sonnet-4-20250514",
				DefaultThinkingLevel: "medium",
				Skills:               []string{"skills/agent-browser"},
			},
		},
	}
}

// Load reads config from the path in AGENT_CONFIG or defaults to ./config.yaml.
func Load() (*Config, error) {
	cfg := Defaults()

	path := os.Getenv("AGENT_CONFIG")
	if path == "" {
		path = "config.yaml"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &cfg, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Preserve default pi settings if user config doesn't specify them
	def := Defaults()
	if cfg.Pi.Settings == nil {
		cfg.Pi.Settings = def.Pi.Settings
	} else if len(cfg.Pi.Settings.Skills) == 0 {
		cfg.Pi.Settings.Skills = def.Pi.Settings.Skills
	}

	return &cfg, nil
}
