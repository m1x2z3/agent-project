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
	// 环境变量名集中管理，避免配置键在不同读取和校验逻辑中拼写不一致。
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

// Config 保存应用和大模型客户端的运行时配置。
//
// 配置值来自进程环境变量、.env 文件或代码默认值，敏感配置不会写入代码默认值。
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

// Load 从项目根目录的 .env 加载配置，并执行启动前校验。
//
// 配置优先级为：进程环境变量 > .env > 代码默认值。
func Load() (Config, error) {
	return LoadFromFile(".env")
}

// LoadFromFile 从指定路径读取 .env 格式的配置，并执行启动前校验。
//
// 文件不存在时不会报错，调用方仍可使用进程环境变量和默认值；文件存在但格式错误时会返回错误。
func LoadFromFile(path string) (Config, error) {
	// .env 是本地开发便利项；不存在时继续使用进程环境变量和默认值。
	fileValues, err := readEnvFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("load env file %q: %w", path, err)
	}

	// 配置优先级：进程环境变量 > .env > 代码默认值。
	cfg, err := loadFromEnvironment(fileValues)
	if err != nil {
		return Config{}, err
	}
	// 在启动阶段失败，避免服务启动后才发现 API Key 或模型配置缺失。
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadFromEnvironment 将文件配置、进程环境变量和默认值合并成 Config。
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

// Validate 检查配置是否满足启动条件。
//
// API Key、模型名和合法的模型服务地址是必填项；温度、超时和监听地址也必须满足基本约束。
func (c Config) Validate() error {
	// 密钥和模型名没有安全的通用默认值，必须由部署环境显式提供。
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

// readEnvFile 解析简单的 KEY=VALUE 格式，不会修改当前进程环境。
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

		// 这里只解析简单的 KEY=VALUE 格式，不修改当前进程的环境变量。
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
	// 显式环境变量优先，便于容器和生产环境覆盖本地 .env。
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
