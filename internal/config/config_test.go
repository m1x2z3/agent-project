package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadUsesDefaults(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_TEMPERATURE", "")
	t.Setenv("LLM_TIMEOUT", "")
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.LLMBaseURL != "https://api.openai.com/v1" {
		t.Fatalf("LLMBaseURL = %q", cfg.LLMBaseURL)
	}
	if cfg.LLMTemperature != 0.2 {
		t.Fatalf("LLMTemperature = %v", cfg.LLMTemperature)
	}
	if cfg.LLMTimeout != 30*time.Second {
		t.Fatalf("LLMTimeout = %v", cfg.LLMTimeout)
	}
}

func TestLoadParsesValues(t *testing.T) {
	t.Setenv("APP_NAME", "test-agent")
	t.Setenv("HTTP_ADDR", "127.0.0.1:9090")
	t.Setenv("LLM_BASE_URL", "http://localhost:9000/v1/")
	t.Setenv("LLM_API_KEY", "secret")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_TEMPERATURE", "0.7")
	t.Setenv("LLM_TIMEOUT", "5s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.AppName != "test-agent" || cfg.HTTPAddr != "127.0.0.1:9090" {
		t.Fatalf("unexpected application config: %+v", cfg)
	}
	if cfg.LLMBaseURL != "http://localhost:9000/v1" {
		t.Fatalf("LLMBaseURL = %q", cfg.LLMBaseURL)
	}
	if cfg.LLMTemperature != 0.7 || cfg.LLMTimeout != 5*time.Second {
		t.Fatalf("unexpected LLM config: %+v", cfg)
	}
}

func TestLoadRejectsInvalidTemperature(t *testing.T) {
	t.Setenv("LLM_TEMPERATURE", "2.1")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
}

func TestLoadRejectsInvalidTimeout(t *testing.T) {
	t.Setenv("LLM_TIMEOUT", "not-a-duration")
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test-model")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want parse error")
	}
}

func TestLoadFromFileReadsDotEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	err := os.WriteFile(envPath, []byte(`
# local model configuration
export APP_NAME="file-agent"
LLM_BASE_URL=http://localhost:9000/v1/
LLM_API_KEY='file-key'
LLM_MODEL=file-model
LLM_TEMPERATURE=0.8
LLM_TIMEOUT=7s
`), 0600)
	if err != nil {
		t.Fatalf("write env file: %v", err)
	}

	cfg, err := LoadFromFile(envPath)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	if cfg.AppName != "file-agent" ||
		cfg.LLMBaseURL != "http://localhost:9000/v1" ||
		cfg.LLMAPIKey != "file-key" ||
		cfg.LLMModel != "file-model" ||
		cfg.LLMTemperature != 0.8 ||
		cfg.LLMTimeout != 7*time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadFromFileKeepsExistingEnvironmentValues(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("LLM_MODEL=from-file\n"), 0600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	t.Setenv("LLM_MODEL", "from-environment")
	t.Setenv("LLM_API_KEY", "test-key")

	cfg, err := LoadFromFile(envPath)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if cfg.LLMModel != "from-environment" {
		t.Fatalf("LLMModel = %q, want environment value", cfg.LLMModel)
	}
}

func TestLoadRejectsMissingRequiredValues(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_MODEL", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want required value error")
	}
}

func TestConfigValidateRejectsInvalidBaseURL(t *testing.T) {
	cfg := Config{
		LLMAPIKey:      "test-key",
		LLMModel:       "test-model",
		LLMBaseURL:     "not-a-url",
		LLMTemperature: 0.2,
		LLMTimeout:     time.Second,
		HTTPAddr:       ":8080",
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid URL error")
	}
}
