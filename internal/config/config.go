package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	envAppName        = "APP_NAME"
	envHTTPAddr       = "HTTP_ADDR"
	envLogLevel       = "LOG_LEVEL"
	envLLMBaseURL     = "LLM_BASE_URL"
	envLLMAPIKey      = "LLM_API_KEY"
	envLLMModel       = "LLM_MODEL"
	envLLMTemperature = "LLM_TEMPERATURE"
	envLLMTimeout     = "LLM_TIMEOUT"

	defaultAppName        = "研发协作 Agent"
	defaultHTTPAddr       = ":8080"
	defaultLogLevel       = "info"
	defaultLLMBaseURL     = "https://api.openai.com/v1"
	defaultLLMTemperature = 0.2
	defaultLLMTimeout     = 30 * time.Second
)

type Config struct {
	AppName        string
	HTTPAddr       string
	LogLevel       string
	LLMBaseURL     string
	LLMAPIKey      string
	LLMModel       string
	LLMTemperature float64
	LLMTimeout     time.Duration
}

func Load() (Config, error) {
	return LoadFromFile(".env")
}

func LoadFromFile(path string) (Config, error) {
	fileValues, err := readEnvFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("load env file %q: %w", path, err)
	}

	cfg, err := loadFromEnvironment(fileValues)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadFromEnvironment(fileValues map[string]string) (Config, error) {
	temperature, err := parseFloat(configValue(envLLMTemperature, fileValues), defaultLLMTemperature)
	if err != nil {
		return Config{}, err
	}
	if temperature < 0 || temperature > 2 {
		return Config{}, fmt.Errorf("%s must be between 0 and 2", envLLMTemperature)
	}

	timeout, err := parseDuration(configValue(envLLMTimeout, fileValues), defaultLLMTimeout)
	if err != nil {
		return Config{}, err
	}
	if timeout <= 0 {
		return Config{}, fmt.Errorf("%s must be greater than zero", envLLMTimeout)
	}

	baseURL := strings.TrimRight(configValueOrDefault(envLLMBaseURL, defaultLLMBaseURL, fileValues), "/")
	if baseURL == "" {
		return Config{}, fmt.Errorf("%s must not be empty", envLLMBaseURL)
	}

	return Config{
		AppName:        configValueOrDefault(envAppName, defaultAppName, fileValues),
		HTTPAddr:       configValueOrDefault(envHTTPAddr, defaultHTTPAddr, fileValues),
		LogLevel:       configValueOrDefault(envLogLevel, defaultLogLevel, fileValues),
		LLMBaseURL:     baseURL,
		LLMAPIKey:      strings.TrimSpace(configValue(envLLMAPIKey, fileValues)),
		LLMModel:       strings.TrimSpace(configValue(envLLMModel, fileValues)),
		LLMTemperature: temperature,
		LLMTimeout:     timeout,
	}, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.LLMAPIKey) == "" {
		return fmt.Errorf("%s is required", envLLMAPIKey)
	}
	if strings.TrimSpace(c.LLMModel) == "" {
		return fmt.Errorf("%s is required", envLLMModel)
	}
	if strings.TrimSpace(c.LLMBaseURL) == "" {
		return fmt.Errorf("%s is required", envLLMBaseURL)
	}

	baseURL, err := url.Parse(c.LLMBaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return fmt.Errorf("LLM_BASE_URL must be a valid URL")
	}
	if c.LLMTemperature < 0 || c.LLMTemperature > 2 {
		return fmt.Errorf("%s must be between 0 and 2", envLLMTemperature)
	}
	if c.LLMTimeout <= 0 {
		return fmt.Errorf("%s must be greater than zero", envLLMTimeout)
	}
	if strings.TrimSpace(c.HTTPAddr) == "" {
		return fmt.Errorf("%s must not be empty", envHTTPAddr)
	}
	return nil
}

func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("invalid entry at line %d", lineNumber)
		}
		key = strings.TrimSpace(key)
		value = parseEnvValue(strings.TrimSpace(value))
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	return values, nil
}

func parseEnvValue(value string) string {
	if len(value) < 2 {
		return value
	}
	if value[0] == '"' && value[len(value)-1] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	return value
}

func configValue(key string, fileValues map[string]string) string {
	if value, exists := os.LookupEnv(key); exists {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(fileValues[key])
}

func configValueOrDefault(key, fallback string, fileValues map[string]string) string {
	value := configValue(key, fileValues)
	if value == "" {
		return fallback
	}
	return value
}

func parseFloat(value string, fallback float64) (float64, error) {
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", envLLMTemperature, err)
	}
	return parsed, nil
}

func parseDuration(value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", envLLMTimeout, err)
	}
	return parsed, nil
}
