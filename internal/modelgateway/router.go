package modelgateway

import (
	"context"
	"log/slog"
	"strings"

	"agent-project/internal/config"
)

// Router 根据显式供应商、任务路由和默认供应商选择模型。
type Router struct {
	providers       map[string]ModelProvider
	defaultProvider string
	taskRoutes      map[string]string
}

// NewRouter 创建模型路由器。
func NewRouter(providers []ModelProvider, defaultProvider string, taskRoutes map[string]string) (*Router, error) {
	if len(providers) == 0 {
		return nil, &Error{Kind: ErrorProviderUnavailable}
	}
	defaultProvider = normalizeName(defaultProvider)
	if defaultProvider == "" {
		return nil, &Error{Kind: ErrorProviderNotFound}
	}

	registered := make(map[string]ModelProvider, len(providers))
	for _, provider := range providers {
		if provider == nil || normalizeName(provider.Name()) == "" {
			return nil, &Error{Kind: ErrorInvalidRequest}
		}
		name := normalizeName(provider.Name())
		if _, exists := registered[name]; exists {
			return nil, &Error{Kind: ErrorInvalidRequest, Provider: name}
		}
		registered[name] = provider
	}
	if _, ok := registered[defaultProvider]; !ok {
		return nil, &Error{Kind: ErrorProviderNotFound, Provider: defaultProvider}
	}

	normalizedRoutes := make(map[string]string, len(taskRoutes))
	for task, provider := range taskRoutes {
		task = strings.ToLower(strings.TrimSpace(task))
		provider = normalizeName(provider)
		if task == "" || provider == "" {
			return nil, &Error{Kind: ErrorInvalidRequest}
		}
		if _, ok := registered[provider]; !ok {
			return nil, &Error{Kind: ErrorProviderNotFound, Provider: provider}
		}
		normalizedRoutes[task] = provider
	}
	return &Router{
		providers:       registered,
		defaultProvider: defaultProvider,
		taskRoutes:      normalizedRoutes,
	}, nil
}

// NewRouterFromConfig 根据配置创建 OpenAI 兼容供应商和路由器。
func NewRouterFromConfig(cfg config.Config, logger *slog.Logger) (*Router, error) {
	providerConfigs := cfg.LLMProviders
	if len(providerConfigs) == 0 {
		providerName := cfg.LLMDefaultProvider
		if providerName == "" {
			providerName = "default"
		}
		providerConfigs = map[string]config.ProviderConfig{
			providerName: {
				Name:        providerName,
				BaseURL:     cfg.LLMBaseURL,
				APIKey:      cfg.LLMAPIKey,
				Model:       cfg.LLMModel,
				Temperature: cfg.LLMTemperature,
				MaxTokens:   cfg.LLMMaxTokens,
				Timeout:     cfg.LLMTimeout,
			},
		}
	}

	providers := make([]ModelProvider, 0, len(providerConfigs))
	for _, providerConfig := range providerConfigs {
		provider, err := NewOpenAICompatibleProvider(providerConfig, logger)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	defaultProvider := cfg.LLMDefaultProvider
	if defaultProvider == "" {
		for name := range providerConfigs {
			defaultProvider = name
			break
		}
	}
	return NewRouter(providers, defaultProvider, cfg.LLMRoutes)
}

// Chat 选择供应商并执行一次调用，不做自动故障转移或重试。
func (r *Router) Chat(ctx context.Context, request ChatRequest) (ChatResponse, error) {
	provider, err := r.selectProvider(request)
	if err != nil {
		return ChatResponse{}, err
	}
	health := provider.Health(ctx)
	if health.Status == ProviderUnhealthy || health.Status == ProviderUnknown {
		return ChatResponse{}, &Error{
			Kind:     ErrorProviderUnavailable,
			Provider: provider.Name(),
		}
	}
	return provider.Chat(ctx, request)
}

// Select 返回本次请求将使用的供应商，便于日志和测试解释路由结果。
func (r *Router) Select(request ChatRequest) (ModelProvider, error) {
	return r.selectProvider(request)
}

func (r *Router) selectProvider(request ChatRequest) (ModelProvider, error) {
	name := normalizeName(request.Provider)
	if name == "" {
		name = r.taskRoutes[strings.ToLower(strings.TrimSpace(request.Task))]
	}
	if name == "" {
		name = r.defaultProvider
	}
	provider, ok := r.providers[name]
	if !ok {
		return nil, &Error{Kind: ErrorProviderNotFound, Provider: name}
	}
	return provider, nil
}

func normalizeName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
