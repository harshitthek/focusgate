package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSimulateDoesNotOpenStorage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "must-not-exist.db")
	cfgPath := filepath.Join(dir, "focusgate.yaml")
	text := "storage:\n  path: \"" + filepath.ToSlash(dbPath) + "\"\nclassifier:\n  mode: hybrid\n"
	if err := os.WriteFile(cfgPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSimulate([]string{"--config", cfgPath, "--title", "Routine update", "--mode", "deep_work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("simulation touched storage: %v", err)
	}
}
