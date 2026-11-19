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
	envLLMMaxTokens   = "LLM_MAX_TOKENS"
	envLLMTimeout     = "LLM_TIMEOUT"
	envLLMProviders   = "LLM_PROVIDERS"
	envLLMDefault     = "LLM_DEFAULT_PROVIDER"

	defaultAppName        = "研发协作 Agent"
	defaultHTTPAddr       = ":8080"
	defaultLogLevel       = "info"
	defaultLLMBaseURL     = "https://api.openai.com/v1"
	defaultLLMTemperature = 0.2
	defaultLLMTimeout     = 30 * time.Second
	defaultProviderName   = "default"
)

// ProviderConfig 保存单个模型供应商的连接和调用参数。
//
// API Key 只用于构造模型客户端，不应被写入日志、错误响应或提交记录。
type ProviderConfig struct {
	Name        string
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	MaxTokens   int
	Timeout     time.Duration
}

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
	LLMMaxTokens   int
	LLMTimeout     time.Duration

	// LLMDefaultProvider 是没有显式指定供应商时使用的供应商。
	LLMDefaultProvider string
	// LLMProviders 保存供应商配置，键为规范化后的供应商名称。
	LLMProviders map[string]ProviderConfig
	// LLMRoutes 按任务名指定供应商，例如 chat -> deepseek。
	LLMRoutes map[string]string
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
	maxTokens, err := parseInt(configValue(envLLMMaxTokens, fileValues), 0)
	if err != nil {
		return Config{}, fmt.Errorf("%s must be an integer: %w", envLLMMaxTokens, err)
	}
	if maxTokens < 0 {
		return Config{}, fmt.Errorf("%s must not be negative", envLLMMaxTokens)
	}

	baseURL := strings.TrimRight(configValueOrDefault(envLLMBaseURL, defaultLLMBaseURL, fileValues), "/")
	if baseURL == "" {
		return Config{}, fmt.Errorf("%s must not be empty", envLLMBaseURL)
	}

	cfg := Config{
		AppName:        configValueOrDefault(envAppName, defaultAppName, fileValues),
		HTTPAddr:       configValueOrDefault(envHTTPAddr, defaultHTTPAddr, fileValues),
		LogLevel:       configValueOrDefault(envLogLevel, defaultLogLevel, fileValues),
		LLMBaseURL:     baseURL,
		LLMAPIKey:      strings.TrimSpace(configValue(envLLMAPIKey, fileValues)),
		LLMModel:       strings.TrimSpace(configValue(envLLMModel, fileValues)),
		LLMTemperature: temperature,
		LLMMaxTokens:   maxTokens,
		LLMTimeout:     timeout,
	}

	providers, defaultProvider, routes, err := loadProviders(fileValues, cfg)
	if err != nil {
		return Config{}, err
	}
	cfg.LLMProviders = providers
	cfg.LLMDefaultProvider = defaultProvider
	cfg.LLMRoutes = routes

	// 将默认供应商同步到旧字段，保证现有 cmd/server 和 internal/llm
	// 仍能使用原有的单客户端启动方式。
	defaultConfig := providers[defaultProvider]
	cfg.LLMBaseURL = defaultConfig.BaseURL
	cfg.LLMAPIKey = defaultConfig.APIKey
	cfg.LLMModel = defaultConfig.Model
	cfg.LLMTemperature = defaultConfig.Temperature
	cfg.LLMMaxTokens = defaultConfig.MaxTokens
	cfg.LLMTimeout = defaultConfig.Timeout
	return cfg, nil
}

// Validate 检查配置是否满足启动条件。
//
// API Key、模型名和合法的模型服务地址是必填项；温度、超时和监听地址也必须满足基本约束。
func (c Config) Validate() error {
	if len(c.LLMProviders) > 0 {
		if strings.TrimSpace(c.LLMDefaultProvider) == "" {
			return errors.New("LLM_DEFAULT_PROVIDER must not be empty")
		}
		if _, ok := c.LLMProviders[c.LLMDefaultProvider]; !ok {
			return fmt.Errorf("LLM_DEFAULT_PROVIDER %q is not configured", c.LLMDefaultProvider)
		}
		for name, provider := range c.LLMProviders {
			if err := provider.Validate(); err != nil {
				return fmt.Errorf("provider %q: %w", name, err)
			}
		}
		for task, provider := range c.LLMRoutes {
			if _, ok := c.LLMProviders[provider]; !ok {
				return fmt.Errorf("LLM_ROUTE_%s references unknown provider %q", strings.ToUpper(task), provider)
			}
		}
	} else if err := validateLegacyLLMConfig(c); err != nil {
		return err
	}
	if c.LLMMaxTokens < 0 {
		return fmt.Errorf("%s must not be negative", envLLMMaxTokens)
	}
	if strings.TrimSpace(c.HTTPAddr) == "" {
		return fmt.Errorf("%s must not be empty", envHTTPAddr)
	}
	return nil
}

// Validate 检查单个供应商配置是否可用于创建模型客户端。
func (c ProviderConfig) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("provider name must not be empty")
	}
	apiKeyEnv := providerEnvKey(c.Name, "API_KEY")
	modelEnv := providerEnvKey(c.Name, "MODEL")
	baseURLEnv := providerEnvKey(c.Name, "BASE_URL")
	temperatureEnv := providerEnvKey(c.Name, "TEMPERATURE")
	timeoutEnv := providerEnvKey(c.Name, "TIMEOUT")
	maxTokensEnv := providerEnvKey(c.Name, "MAX_TOKENS")
	if c.Name == defaultProviderName {
		apiKeyEnv = envLLMAPIKey
		modelEnv = envLLMModel
		baseURLEnv = envLLMBaseURL
		temperatureEnv = envLLMTemperature
		timeoutEnv = envLLMTimeout
		maxTokensEnv = envLLMMaxTokens
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("%s is required", apiKeyEnv)
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("%s is required", modelEnv)
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return fmt.Errorf("%s is required", baseURLEnv)
	}
	baseURL, err := url.Parse(c.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return fmt.Errorf("%s must be a valid URL", baseURLEnv)
	}
	if c.Temperature < 0 || c.Temperature > 2 {
		return fmt.Errorf("%s must be between 0 and 2", temperatureEnv)
	}
	if c.MaxTokens < 0 {
		return fmt.Errorf("%s must not be negative", maxTokensEnv)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("%s must be greater than zero", timeoutEnv)
	}
	return nil
}

func validateLegacyLLMConfig(c Config) error {
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
	return nil
}

func loadProviders(fileValues map[string]string, legacy Config) (map[string]ProviderConfig, string, map[string]string, error) {
	rawProviders := configValue(envLLMProviders, fileValues)
	providerNames := splitList(rawProviders)
	multiProviderMode := rawProviders != ""
	if len(providerNames) == 0 {
		providerNames = []string{defaultProviderName}
	}

	providers := make(map[string]ProviderConfig, len(providerNames))
	for _, rawName := range providerNames {
		name, err := normalizeProviderName(rawName)
		if err != nil {
			return nil, "", nil, err
		}
		if _, exists := providers[name]; exists {
			return nil, "", nil, fmt.Errorf("duplicate provider %q", name)
		}

		provider := ProviderConfig{
			Name:        name,
			BaseURL:     legacy.LLMBaseURL,
			APIKey:      legacy.LLMAPIKey,
			Model:       legacy.LLMModel,
			Temperature: legacy.LLMTemperature,
			MaxTokens:   legacy.LLMMaxTokens,
			Timeout:     legacy.LLMTimeout,
		}
		if multiProviderMode {
			provider, err = loadProviderFromEnvironment(name, fileValues)
			if err != nil {
				return nil, "", nil, err
			}
		}
		providers[name] = provider
	}

	defaultProvider := strings.TrimSpace(configValue(envLLMDefault, fileValues))
	if defaultProvider == "" {
		defaultProvider = providerNames[0]
	}
	defaultProvider, err := normalizeProviderName(defaultProvider)
	if err != nil {
		return nil, "", nil, fmt.Errorf("%s: %w", envLLMDefault, err)
	}
	if _, ok := providers[defaultProvider]; !ok {
		return nil, "", nil, fmt.Errorf("%s %q is not configured", envLLMDefault, defaultProvider)
	}

	routes, err := loadRoutes(fileValues)
	if err != nil {
		return nil, "", nil, err
	}
	return providers, defaultProvider, routes, nil
}

func loadProviderFromEnvironment(name string, fileValues map[string]string) (ProviderConfig, error) {
	temperature, err := parseFloat(configValue(providerEnvKey(name, "TEMPERATURE"), fileValues), defaultLLMTemperature)
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("%s must be a number: %w", providerEnvKey(name, "TEMPERATURE"), err)
	}
	timeout, err := parseDuration(configValue(providerEnvKey(name, "TIMEOUT"), fileValues), defaultLLMTimeout)
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("%s must be a duration: %w", providerEnvKey(name, "TIMEOUT"), err)
	}
	maxTokens, err := parseInt(configValue(providerEnvKey(name, "MAX_TOKENS"), fileValues), 0)
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("%s must be an integer: %w", providerEnvKey(name, "MAX_TOKENS"), err)
	}

	return ProviderConfig{
		Name:        name,
		BaseURL:     strings.TrimRight(configValue(providerEnvKey(name, "BASE_URL"), fileValues), "/"),
		APIKey:      strings.TrimSpace(configValue(providerEnvKey(name, "API_KEY"), fileValues)),
		Model:       strings.TrimSpace(configValue(providerEnvKey(name, "MODEL"), fileValues)),
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Timeout:     timeout,
	}, nil
}

func loadRoutes(fileValues map[string]string) (map[string]string, error) {
	keys := make(map[string]struct{})
	for key := range fileValues {
		if strings.HasPrefix(key, "LLM_ROUTE_") {
			keys[key] = struct{}{}
		}
	}
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "LLM_ROUTE_") {
			keys[key] = struct{}{}
		}
	}

	routes := make(map[string]string, len(keys))
	for key := range keys {
		task := strings.ToLower(strings.TrimPrefix(key, "LLM_ROUTE_"))
		if task == "" {
			return nil, fmt.Errorf("%s must include a task name", key)
		}
		provider := strings.TrimSpace(configValue(key, fileValues))
		if provider == "" {
			return nil, fmt.Errorf("%s must not be empty", key)
		}
		normalized, err := normalizeProviderName(provider)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		routes[task] = normalized
	}
	return routes, nil
}

func providerEnvKey(name, field string) string {
	return "LLM_" + strings.ToUpper(name) + "_" + field
}

func normalizeProviderName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", errors.New("provider name must not be empty")
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return "", fmt.Errorf("invalid provider name %q", name)
		}
	}
	return name, nil
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
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

func parseInt(value string, fallback int) (int, error) {
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}
