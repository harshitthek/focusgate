package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_EnvExpansionAndValidation(t *testing.T) {
	t.Setenv("FG_TEST_TOKEN", "secret-abc")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "focusgate.yaml")
	yamlBody := `
server:
  listen: "127.0.0.1:9090"
  auth_token: "${FG_TEST_TOKEN}"
focus:
  default_mode: "deep_work"
  digest_times: [" 9:05 ", "20:15"]
rules:
  - name: "Outage"
    regex: "(?i)outage"
    action: "push_now"
    priority: 5
`
	if err := os.WriteFile(cfgPath, []byte(yamlBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load valid config failed: %v", err)
	}
	if cfg.Server.AuthToken != "secret-abc" {
		t.Fatalf("expected expanded token secret-abc, got %q", cfg.Server.AuthToken)
	}
	if cfg.Focus.DefaultMode != ModeDeepWork {
		t.Fatalf("expected deep_work, got %v", cfg.Focus.DefaultMode)
	}
	if cfg.Focus.DigestTimes[0] != "09:05" {
		t.Fatalf("expected canonical digest time 09:05, got %q", cfg.Focus.DigestTimes[0])
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].CompiledRegex == nil {
		t.Fatalf("expected compiled regex in rule, got %+v", cfg.Rules)
	}

	// Reject invalid regex and invalid digest schedule.
	badCfg := Default()
	badCfg.Focus.DigestTimes = []string{"25:99"}
	if err := badCfg.NormalizeAndValidate(); err == nil {
		t.Fatal("expected invalid digest time to fail validation")
	}

	badRegex := Default()
	badRegex.Rules = []RuleConfig{{Name: "Bad", Regex: "[unclosed", Action: "push_now", Priority: 3}}
	if err := badRegex.NormalizeAndValidate(); err == nil {
		t.Fatal("expected invalid regex to fail validation")
	}

	// Non-loopback bind without auth and webhook secret must fail validation.
	openListen := Default()
	openListen.Server.Listen = "0.0.0.0:8787"
	if err := openListen.NormalizeAndValidate(); err == nil {
		t.Fatal("expected non-loopback listen without auth_token to fail validation")
	}
	openListen.Server.AuthToken = "ui-token"
	if err := openListen.NormalizeAndValidate(); err == nil {
		t.Fatal("expected non-loopback listen with enabled webhook and empty secret to fail")
	}
	openListen.Sources.Webhook.Secret = "hook-secret"
	if err := openListen.NormalizeAndValidate(); err != nil {
		t.Fatalf("expected non-loopback listen with both secrets to pass, got %v", err)
	}

	// Reject placeholder ntfy topic when ntfy is enabled.
	placeholderNtfy := Default()
	placeholderNtfy.Notify.Ntfy.Enabled = true
	placeholderNtfy.Notify.Ntfy.Topic = "focusgate-change-this-secret-topic"
	if err := placeholderNtfy.NormalizeAndValidate(); err == nil {
		t.Fatal("expected placeholder ntfy topic to fail validation")
	}
}
