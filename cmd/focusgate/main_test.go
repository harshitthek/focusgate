package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/store"
	"gopkg.in/yaml.v3"
)

func TestSimulateDoesNotOpenStorage(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		dir = filepath.Join(dir, "quoted\"directory")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(dir, "must-not-exist.db")
	cfgPath := filepath.Join(dir, "focusgate.yaml")
	text, err := yaml.Marshal(map[string]any{"storage": map[string]any{"path": dbPath}, "classifier": map[string]any{"mode": "hybrid"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, text, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSimulate([]string{"--config", cfgPath, "--title", "Routine update", "--mode", "deep_work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("simulation touched storage: %v", err)
	}
}

func TestSemanticSimulationUsesPersistedMode(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetFocusMode(context.Background(), config.ModeDND); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	text, err := yaml.Marshal(map[string]any{
		"storage":    map[string]any{"path": dbPath},
		"classifier": map[string]any{"mode": "rules_only"},
		"rules":      []map[string]any{{"name": "direct", "contains": "Review", "action": "push_now", "priority": 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, text, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, route string }{{"", "queue_digest"}, {"normal", "push_now"}} {
		args := []string{"--config", cfgPath, "--title", "Review needed", "--semantic"}
		if tc.mode != "" {
			args = append(args, "--mode", tc.mode)
		}
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout := os.Stdout
		os.Stdout = write
		err = runSimulate(args)
		os.Stdout = stdout
		_ = write.Close()
		output, readErr := io.ReadAll(read)
		_ = read.Close()
		if err != nil || readErr != nil {
			t.Fatalf("simulate: %v, read: %v", err, readErr)
		}
		var decision classify.Decision
		if err := json.Unmarshal(output, &decision); err != nil || decision.Route != tc.route {
			t.Fatalf("mode %q: got %s (%v)", tc.mode, output, err)
		}
	}
	for _, semantic := range []bool{false, true} {
		args := []string{"--config", cfgPath, "--title", "Review", "--mode", "invalid"}
		if semantic {
			args = append(args, "--semantic")
		}
		if err := runSimulate(args); err == nil {
			t.Fatal("invalid explicit focus mode accepted")
		}
	}
}
