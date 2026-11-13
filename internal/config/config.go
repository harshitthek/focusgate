package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FocusMode governs how aggressively incoming items interrupt the user.
type FocusMode string

const (
	ModeNormal   FocusMode = "normal"
	ModeDeepWork FocusMode = "deep_work"
	ModeDND      FocusMode = "dnd"
)

// ValidFocusMode reports whether mode is one of the supported focus profiles.
func ValidFocusMode(mode FocusMode) bool {
	return mode == ModeNormal || mode == ModeDeepWork || mode == ModeDND
}

// Config captures all runtime settings for the focusgate daemon.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Storage    StorageConfig    `yaml:"storage"`
	Focus      FocusConfig      `yaml:"focus"`
	Classifier ClassifierConfig `yaml:"classifier"`
	Notify     NotifyConfig     `yaml:"notify"`
	Sources    SourcesConfig    `yaml:"sources"`
	Rules      []RuleConfig     `yaml:"rules"`
}

type ServerConfig struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url"`
	AuthToken string `yaml:"auth_token"`
}

type StorageConfig struct {
	Path            string `yaml:"path"`
	RetentionDays   int    `yaml:"retention_days"`
	MaxSnippetChars int    `yaml:"max_snippet_chars"`
}

type FocusConfig struct {
	DefaultMode FocusMode `yaml:"default_mode"`
	DigestTimes []string  `yaml:"digest_times"`
	UserContext string    `yaml:"user_context"`
}

type ClassifierConfig struct {
	Mode             string `yaml:"mode"`
	TypeSafeAPIKey   string `yaml:"typesafe_api_key"`
	TypeSafeBaseURL  string `yaml:"typesafe_base_url"`
	Model            string `yaml:"model"`
	TimeoutSeconds   int    `yaml:"timeout_seconds"`
	MaxDailyAPICalls int    `yaml:"max_daily_api_calls"`
}

type NotifyConfig struct {
	Ntfy           NtfyConfig           `yaml:"ntfy"`
	DesktopWindows DesktopWindowsConfig `yaml:"desktop_windows"`
	Slack          SlackConfig          `yaml:"slack"`
	Matrix         MatrixConfig         `yaml:"matrix"`
	OnlyMentions   bool                 `yaml:"only_mentions"`
	MentionHandles []string             `yaml:"mention_handles"`
}

type NtfyConfig struct {
	Enabled bool   `yaml:"enabled"`
	Server  string `yaml:"server"`
	Topic   string `yaml:"topic"`
	Token   string `yaml:"token"`
}

type DesktopWindowsConfig struct {
	Enabled bool `yaml:"enabled"`
}

type SlackConfig struct {
	Enabled       bool   `yaml:"enabled"`
	WebhookURL    string `yaml:"webhook_url"`
	MentionHandle string `yaml:"mention_handle"`
}

type MatrixConfig struct {
	Enabled       bool   `yaml:"enabled"`
	WebhookURL    string `yaml:"webhook_url"`
	MentionHandle string `yaml:"mention_handle"`
}

type SourcesConfig struct {
	RSS     []RSSSourceConfig `yaml:"rss"`
	GitHub  GitHubConfig      `yaml:"github"`
	IMAP    []IMAPConfig      `yaml:"imap"`
	Webhook WebhookConfig     `yaml:"webhook"`
}

type RSSSourceConfig struct {
	Name        string `yaml:"name"`
	URL         string `yaml:"url"`
	Tag         string `yaml:"tag"`
	PollMinutes int    `yaml:"poll_minutes"`
}

type GitHubConfig struct {
	Enabled           bool   `yaml:"enabled"`
	Token             string `yaml:"token"`
	PollMinutes       int    `yaml:"poll_minutes"`
	ParticipatingOnly bool   `yaml:"participating_only"`
}

type IMAPConfig struct {
	Name        string `yaml:"name"`
	Enabled     bool   `yaml:"enabled"`
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	Mailbox     string `yaml:"mailbox"`
	PollMinutes int    `yaml:"poll_minutes"`
}

type WebhookConfig struct {
	Enabled bool   `yaml:"enabled"`
	Secret  string `yaml:"secret"`
}

type RuleConfig struct {
	Name          string `yaml:"name"`
	SourceType    string `yaml:"source_type"`
	SourceName    string `yaml:"source_name"`
	SenderMatch   string `yaml:"sender_contains"`
	Contains      string `yaml:"contains"`
	Regex         string `yaml:"regex"`
	GitHubReason  string `yaml:"github_reason"`
	Action        string `yaml:"action"`
	Priority      int    `yaml:"priority"`
	CompiledRegex *regexp.Regexp
}

// Default returns a minimal low-footprint configuration suitable for local startup.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Listen:    "127.0.0.1:8787",
			PublicURL: "http://127.0.0.1:8787",
		},
		Storage: StorageConfig{
			Path:            "./data/focusgate.db",
			RetentionDays:   14,
			MaxSnippetChars: 1200,
		},
		Focus: FocusConfig{
			DefaultMode: ModeNormal,
			DigestTimes: []string{"09:00", "13:30", "19:30"},
			UserContext: "Software developer; urgent outages, direct mentions, and personal blockers matter.",
		},
		Classifier: ClassifierConfig{
			Mode:             "hybrid",
			TypeSafeBaseURL:  "https://api.typesafe.ai",
			Model:            "jev-latest",
			TimeoutSeconds:   10,
			MaxDailyAPICalls: 200,
		},
		Notify: NotifyConfig{
			Ntfy:           NtfyConfig{Server: "https://ntfy.sh"},
			DesktopWindows: DesktopWindowsConfig{Enabled: true},
		},
		Sources: SourcesConfig{
			Webhook: WebhookConfig{Enabled: true},
		},
	}
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnvNode(node *yaml.Node) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode {
		orig := node.Value
		node.Value = envPattern.ReplaceAllStringFunc(node.Value, func(match string) string {
			key := match[2 : len(match)-1]
			return os.Getenv(key)
		})
		if node.Value != orig && (node.Style == 0 || node.Style == yaml.TaggedStyle) {
			var tmp yaml.Node
			if err := yaml.Unmarshal([]byte(node.Value), &tmp); err == nil && len(tmp.Content) > 0 {
				resolved := tmp.Content[0]
				if resolved.Kind == yaml.ScalarNode && resolved.Tag != "" {
					node.Tag = resolved.Tag
				}
			}
		}
	}
	for _, child := range node.Content {
		expandEnvNode(child)
	}
}

// Load reads a YAML configuration file, expands ${ENV_VAR} placeholders within
// parsed YAML scalar values (preserving backslashes and quotes without breaking YAML syntax),
// applies defaults, and validates rules and schedules.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
		var root yaml.Node
		if err := yaml.Unmarshal(raw, &root); err != nil {
			return Config{}, fmt.Errorf("parse yaml %q: %w", path, err)
		}
		expandEnvNode(&root)
		if err := root.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("decode yaml %q: %w", path, err)
		}
	}
	if err := cfg.NormalizeAndValidate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// NormalizeAndValidate fills missing defaults and validates fields fail-fast.
func (c *Config) NormalizeAndValidate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		c.Server.Listen = "127.0.0.1:8787"
	}
	listenHost, _, err := net.SplitHostPort(strings.TrimSpace(c.Server.Listen))
	if err != nil {
		return fmt.Errorf("invalid server.listen %q: %w", c.Server.Listen, err)
	}
	if strings.TrimSpace(c.Server.PublicURL) == "" {
		c.Server.PublicURL = "http://" + c.Server.Listen
	}
	c.Server.PublicURL = strings.TrimRight(c.Server.PublicURL, "/")

	publicHost := ""
	if parsedURL, err := url.Parse(c.Server.PublicURL); err == nil {
		publicHost = parsedURL.Hostname()
	}

	needsAuth := !isLoopbackHost(listenHost) || (publicHost != "" && !isLoopbackHost(publicHost))
	if needsAuth && strings.TrimSpace(c.Server.AuthToken) == "" {
		return fmt.Errorf("server.auth_token is required when server.listen (%q) or server.public_url (%q) is non-loopback", c.Server.Listen, c.Server.PublicURL)
	}

	if !isLoopbackHost(listenHost) {
		if c.Sources.Webhook.Enabled && strings.TrimSpace(c.Sources.Webhook.Secret) == "" {
			return fmt.Errorf("sources.webhook.secret is required when server.listen (%q) is non-loopback and webhooks are enabled", c.Server.Listen)
		}
	}

	if strings.TrimSpace(c.Storage.Path) == "" {
		c.Storage.Path = "./data/focusgate.db"
	}
	c.Storage.Path = filepath.Clean(c.Storage.Path)
	if c.Storage.RetentionDays <= 0 {
		c.Storage.RetentionDays = 14
	}
	if c.Storage.MaxSnippetChars <= 0 {
		c.Storage.MaxSnippetChars = 1200
	}

	if c.Focus.DefaultMode == "" {
		c.Focus.DefaultMode = ModeNormal
	}
	if !ValidFocusMode(c.Focus.DefaultMode) {
		return fmt.Errorf("invalid focus.default_mode %q (expected normal, deep_work, or dnd)", c.Focus.DefaultMode)
	}
	for i, ts := range c.Focus.DigestTimes {
		t, err := time.Parse("15:04", strings.TrimSpace(ts))
		if err != nil {
			return fmt.Errorf("invalid focus.digest_times entry %q: expected HH:MM", ts)
		}
		c.Focus.DigestTimes[i] = t.Format("15:04")
	}

	if c.Classifier.Mode == "" {
		c.Classifier.Mode = "hybrid"
	}
	if c.Classifier.Mode != "hybrid" && c.Classifier.Mode != "rules_only" {
		return fmt.Errorf("invalid classifier.mode %q (expected hybrid or rules_only)", c.Classifier.Mode)
	}
	if c.Classifier.TypeSafeBaseURL == "" {
		c.Classifier.TypeSafeBaseURL = "https://api.typesafe.ai"
	}
	c.Classifier.TypeSafeBaseURL = strings.TrimRight(c.Classifier.TypeSafeBaseURL, "/")
	if c.Classifier.Model == "" {
		c.Classifier.Model = "jev-latest"
	}
	if c.Classifier.TimeoutSeconds <= 0 {
		c.Classifier.TimeoutSeconds = 10
	}
	if c.Classifier.MaxDailyAPICalls <= 0 {
		c.Classifier.MaxDailyAPICalls = 200
	}

	if c.Notify.Ntfy.Enabled {
		topic := strings.TrimSpace(c.Notify.Ntfy.Topic)
		if topic == "" {
			return errors.New("notify.ntfy.topic is required when ntfy is enabled")
		}
		if topic == "focusgate-change-this-secret-topic" {
			return errors.New("notify.ntfy.topic still uses the example placeholder; choose an unguessable topic")
		}
		if strings.TrimSpace(c.Notify.Ntfy.Server) == "" {
			c.Notify.Ntfy.Server = "https://ntfy.sh"
		}
		c.Notify.Ntfy.Server = strings.TrimRight(c.Notify.Ntfy.Server, "/")
	}

	for i := range c.Sources.RSS {
		if c.Sources.RSS[i].PollMinutes <= 0 {
			c.Sources.RSS[i].PollMinutes = 30
		}
		if strings.TrimSpace(c.Sources.RSS[i].URL) == "" {
			return fmt.Errorf("sources.rss[%d].url cannot be empty", i)
		}
		if strings.TrimSpace(c.Sources.RSS[i].Name) == "" {
			c.Sources.RSS[i].Name = c.Sources.RSS[i].URL
		}
	}
	if c.Sources.GitHub.PollMinutes <= 0 {
		c.Sources.GitHub.PollMinutes = 5
	}
	for i := range c.Sources.IMAP {
		if c.Sources.IMAP[i].Port <= 0 {
			c.Sources.IMAP[i].Port = 993
		}
		if c.Sources.IMAP[i].Mailbox == "" {
			c.Sources.IMAP[i].Mailbox = "INBOX"
		}
		if c.Sources.IMAP[i].PollMinutes <= 0 {
			c.Sources.IMAP[i].PollMinutes = 5
		}
	}

	for i := range c.Rules {
		r := &c.Rules[i]
		switch r.Action {
		case "push_now", "queue_digest", "archive", "smart":
		default:
			return fmt.Errorf("rules[%d] (%s): invalid action %q", i, r.Name, r.Action)
		}
		if r.Priority < 0 || r.Priority > 5 {
			return fmt.Errorf("rules[%d] (%s): priority must be 0..5", i, r.Name)
		}
		if r.Regex != "" {
			re, err := regexp.Compile(r.Regex)
			if err != nil {
				return fmt.Errorf("rules[%d] (%s): invalid regex: %w", i, r.Name, err)
			}
			r.CompiledRegex = re
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
