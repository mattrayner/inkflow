package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"inkflow/internal/config"
)

func TestResolveAPIKeyPrefersEnv(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "from-env")
	got, err := resolveGeminiAPIKey(config.GeminiConfig{APIKeyFile: "/does/not/exist"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-env" {
		t.Errorf("got %q", got)
	}
}

func TestResolveAPIKeyFallsBackToFile(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveGeminiAPIKey(config.GeminiConfig{APIKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-file" {
		t.Errorf("got %q", got)
	}
}

func TestResolveAPIKeyMissingErrors(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	_, err := resolveGeminiAPIKey(config.GeminiConfig{})
	if err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

func TestLoadRuntimeOpenAIWithEnvKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "from-env")
	t.Setenv("GEMINI_API_KEY", "")
	rt := loadOpenAIRuntime(t, "")
	if rt.cfg.AI.Provider != "openai" {
		t.Errorf("provider = %q, want openai", rt.cfg.AI.Provider)
	}
}

func TestLoadRuntimeOpenAIMissingKeyErrors(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	dir := t.TempDir()
	configPath := writeRuntimeConfig(t, dir, true, "")
	_, err := loadRuntime(slog.Default(), configPath)
	if err == nil || !strings.Contains(err.Error(), "openai: no API key — set $OPENAI_API_KEY or [openai].api_key_file") {
		t.Fatalf("expected OpenAI missing-key error, got %v", err)
	}
}

func TestLoadRuntimeOpenAIWithKeyFile(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "openai-key")
	if err := os.WriteFile(keyPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loadOpenAIRuntime(t, keyPath)
}

func TestLoadRuntimeOpenAIWithoutAIRouteSkipsKeyResolution(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	dir := t.TempDir()
	configPath := writeRuntimeConfig(t, dir, false, "")
	rt, err := loadRuntime(slog.Default(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.store.Close() })
}

func loadOpenAIRuntime(t *testing.T, keyPath string) runtime {
	t.Helper()
	dir := t.TempDir()
	configPath := writeRuntimeConfig(t, dir, true, keyPath)
	rt, err := loadRuntime(slog.Default(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.store.Close() })
	return rt
}

func writeRuntimeConfig(t *testing.T, dir string, wantsAI bool, keyPath string) string {
	t.Helper()
	configPath := filepath.Join(dir, "inkflow.toml")
	contents := "vault_dir = \"" + filepath.Join(dir, "vault") + "\"\n" +
		"state_file = \"state.db\"\n\n" +
		"[ai]\nprovider = \"openai\"\n\n"
	if keyPath != "" {
		contents += "[openai]\napi_key_file = \"" + keyPath + "\"\n\n"
	}
	contents += "[[route]]\nfrom = \"/uploads\"\nai = "
	if wantsAI {
		contents += "true\n"
	} else {
		contents += "false\n"
	}
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestAnyRouteWantsAIDetectsFlag(t *testing.T) {
	if anyRouteWantsAI(nil) {
		t.Error("nil routes should not want AI")
	}
	if !anyRouteWantsAI([]config.Route{{AI: false}, {AI: true}}) {
		t.Error("one AI route should enable provider")
	}
	if anyRouteWantsAI([]config.Route{{AI: false}, {AI: false}}) {
		t.Error("no AI routes should not enable provider")
	}
}
