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

	// Environment variables containing backslashes and quotes must be preserved as scalar strings
	t.Setenv("FG_TEST_SPECIAL", `path\with\backslashes and "double-quotes" and 'single-quotes'`)
	specialPath := filepath.Join(dir, "special.yaml")
	specialYAML := `
server:
  listen: "127.0.0.1:9091"
  auth_token: "${FG_TEST_SPECIAL}"
`
	if err := os.WriteFile(specialPath, []byte(specialYAML), 0o600); err != nil {
		t.Fatalf("write special config: %v", err)
	}
	specialCfg, err := Load(specialPath)
	if err != nil {
		t.Fatalf("Load with special characters failed: %v", err)
	}
	expectedSpecial := `path\with\backslashes and "double-quotes" and 'single-quotes'`
	if specialCfg.Server.AuthToken != expectedSpecial {
		t.Fatalf("expected %q, got %q", expectedSpecial, specialCfg.Server.AuthToken)
	}

	// Environment variables for integer and boolean fields must expand into typed values
	t.Setenv("FG_TEST_MAX_CALLS", "350")
	t.Setenv("FG_TEST_WEBHOOK_ENABLED", "true")
	typedPath := filepath.Join(dir, "typed.yaml")
	typedYAML := `
classifier:
  max_daily_api_calls: ${FG_TEST_MAX_CALLS}
sources:
  webhook:
    enabled: ${FG_TEST_WEBHOOK_ENABLED}
`
	if err := os.WriteFile(typedPath, []byte(typedYAML), 0o600); err != nil {
		t.Fatalf("write typed config: %v", err)
	}
	typedCfg, err := Load(typedPath)
	if err != nil {
		t.Fatalf("Load with typed env vars failed: %v", err)
	}
	if typedCfg.Classifier.MaxDailyAPICalls != 350 {
		t.Fatalf("expected max_daily_api_calls 350, got %d", typedCfg.Classifier.MaxDailyAPICalls)
	}
	if !typedCfg.Sources.Webhook.Enabled {
		t.Fatalf("expected sources.webhook.enabled true, got %v", typedCfg.Sources.Webhook.Enabled)
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

	// Non-loopback public URL with loopback listen must also require auth_token.
	openPublic := Default()
	openPublic.Server.Listen = "127.0.0.1:8787"
	openPublic.Server.PublicURL = "https://focusgate.remote.com"
	if err := openPublic.NormalizeAndValidate(); err == nil {
		t.Fatal("expected non-loopback public_url without auth_token to fail validation")
	}
	openListen.Server.AuthToken = "ui-token"
	if err := openListen.NormalizeAndValidate(); err == nil {
		t.Fatal("expected non-loopback listen with enabled webhook and empty secret to fail")
	}
	openListen.Sources.Webhook.Secret = "hook-secret"
	if err := openListen.NormalizeAndValidate(); err != nil {
		t.Fatalf("expected non-loopback listen with both secrets to pass, got %v", err)
	}
	openPublic.Server.AuthToken = "ui-token"
	if err := openPublic.NormalizeAndValidate(); err == nil {
		t.Fatal("expected proxied loopback webhook without secret to fail")
	}
	openPublic.Sources.Webhook.Secret = "hook-secret"
	if err := openPublic.NormalizeAndValidate(); err != nil {
		t.Fatalf("expected proxied webhook with both secrets to pass: %v", err)
	}

	// Reject placeholder ntfy topic when ntfy is enabled.
	placeholderNtfy := Default()
	placeholderNtfy.Notify.Ntfy.Enabled = true
	placeholderNtfy.Notify.Ntfy.Topic = "focusgate-change-this-secret-topic"
	if err := placeholderNtfy.NormalizeAndValidate(); err == nil {
		t.Fatal("expected placeholder ntfy topic to fail validation")
	}
}

func TestPublicURLMustBeAbsoluteHTTP(t *testing.T) {
	for _, raw := range []string{"/relative", "https://", "ssh://host.test", "https://host.test/%xx", "https://user:password@host.test", "https://host.test/?token=value"} {
		cfg := Default()
		cfg.Server.PublicURL = raw
		if err := cfg.NormalizeAndValidate(); err == nil {
			t.Errorf("accepted invalid public URL %q", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8787", "http://[::1]:8787", "https://host.test"} {
		cfg := Default()
		cfg.Server.PublicURL = raw
		cfg.Server.AuthToken = "test-token"
		cfg.Sources.Webhook.Secret = "test-secret"
		if err := cfg.NormalizeAndValidate(); err != nil {
			t.Errorf("rejected valid public URL %q: %v", raw, err)
		}
	}
}

func TestArchiveRulesRejectTyposAndEmptySelectorEnvironment(t *testing.T) {
	t.Setenv("FG_RULE_SELECTOR", "")
	cases := []struct {
		name, text string
		valid      bool
	}{
		{"typo", "rules: [{action: archive, sender_contians: boss}]", false},
		{"empty-env", "rules: [{action: archive, sender_contains: '${FG_RULE_SELECTOR}'}]", false},
		{"alias-env", "selector: &s '${FG_RULE_SELECTOR}'\nrules: [{action: archive, sender_contains: *s}]", false},
		{"merged-root-typo", "defaults: &d {rules: [{action: archive, sender_contians: boss}]}\n<<: *d", false},
		{"merged-empty-env", "defaults: &d {rules: [{action: archive, contains: 'prefix-${FG_RULE_SELECTOR}'}]}\n<<: *d", false},
		{"overridden-rules", "defaults: &d {rules: [{action: archive, contains: '${FG_RULE_SELECTOR}'}]}\n<<: *d\nrules: [{action: queue_digest}]", true},
		{"overridden-selector", "defaults: &d {contains: '${FG_RULE_SELECTOR}'}\nrules: [{<<: *d, action: archive, contains: valid}]", true},
		{"null-selector", "rules: [{action: archive, contains: null}]", false},
		{"space-selector", "rules: [{action: archive, contains: '  '}]", false},
		{"push-typo", "rules: [{action: push_now, reciever: boss}]", false},
		{"catch-all", "unrelated_optional: true\nrules: [{name: explicit-catch-all, action: archive}]", true},
		{"valid-selector", "rules: [{action: archive, sender_contains: boss}]", true},
		{"optional-env", "notify: {slack: {webhook_url: '${FG_RULE_SELECTOR}'}}", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rules.yaml")
			if err := os.WriteFile(path, []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
