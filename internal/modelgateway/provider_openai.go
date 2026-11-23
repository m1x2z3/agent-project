package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"agent-project/internal/config"
	"agent-project/internal/llm"
)

// OpenAICompatibleProvider 将统一模型接口适配到现有 internal/llm.Client。
type OpenAICompatibleProvider struct {
	config config.ProviderConfig
	client openAICompatibleClient
}

// openAICompatibleClient 是 OpenAI 兼容 Provider 依赖的底层客户端接口。
//
// 它隔离具体的 llm.Client，便于在不访问真实模型服务的情况下测试该 Provider。
type openAICompatibleClient interface {
	ChatWithOptions(context.Context, []llm.Message, llm.ChatOptions) (llm.ChatResponse, error)
}

// NewOpenAICompatibleProvider 创建一个 OpenAI 兼容供应商适配器。
func NewOpenAICompatibleProvider(cfg config.ProviderConfig, logger *slog.Logger) (*OpenAICompatibleProvider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate provider %q: %w", cfg.Name, err)
	}
	return &OpenAICompatibleProvider{
		config: cfg,
		client: llm.NewClient(config.Config{
			LLMBaseURL:     cfg.BaseURL,
			LLMAPIKey:      cfg.APIKey,
			LLMModel:       cfg.Model,
			LLMTemperature: cfg.Temperature,
			LLMMaxTokens:   cfg.MaxTokens,
			LLMTimeout:     cfg.Timeout,
		}, logger),
	}, nil
}

func (p *OpenAICompatibleProvider) Name() string {
	return p.config.Name
}

// Health 只检查配置，不通过真实模型调用探活。
func (p *OpenAICompatibleProvider) Health(ctx context.Context) ProviderHealth {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ProviderHealth{Status: ProviderUnknown, Reason: "health check canceled by deadline"}
		}
		return ProviderHealth{Status: ProviderUnknown, Reason: "health check canceled"}
	}
	if err := p.config.Validate(); err != nil {
		return ProviderHealth{Status: ProviderUnhealthy, Reason: "provider configuration is invalid"}
	}
	return ProviderHealth{Status: ProviderConfigured, Reason: "provider configuration is valid"}
}

// Chat 将供应商无关请求转换为 internal/llm 请求，并隐藏供应商错误细节。
func (p *OpenAICompatibleProvider) Chat(ctx context.Context, request ChatRequest) (ChatResponse, error) {
	if err := validateChatRequest(request); err != nil {
		return ChatResponse{}, err
	}

	messages := make([]llm.Message, len(request.Messages))
	for i, message := range request.Messages {
		messages[i] = llm.Message{Role: message.Role, Content: message.Content}
	}
	callCtx := ctx
	var cancel context.CancelFunc
	if request.Timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}
	response, err := p.client.ChatWithOptions(callCtx, messages, llm.ChatOptions{
		Model:       request.Model,
		Temperature: request.Temperature,
		MaxTokens:   request.MaxTokens,
	})
	if err != nil {
		return ChatResponse{}, classifyProviderError(p.Name(), err)
	}
	if len(response.Choices) == 0 {
		return ChatResponse{}, &Error{
			Kind:     ErrorInternal,
			Provider: p.Name(),
		}
	}

	choice := response.Choices[0]
	result := ChatResponse{
		ID:       response.ID,
		Provider: p.Name(),
		Model:    response.Model,
		Message: Message{
			Role:    choice.Message.Role,
			Content: choice.Message.Content,
		},
		FinishReason: choice.FinishReason,
		Usage: &Usage{
			PromptTokens:     response.Usage.PromptTokens,
			CompletionTokens: response.Usage.CompletionTokens,
			TotalTokens:      response.Usage.TotalTokens,
		},
	}
	if result.Model == "" {
		result.Model = p.config.Model
	}
	return result, nil
}

func validateChatRequest(request ChatRequest) error {
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		return &Error{Kind: ErrorInvalidRequest}
	}
	if request.MaxTokens != nil && *request.MaxTokens < 0 {
		return &Error{Kind: ErrorInvalidRequest}
	}
	if request.Timeout < 0 {
		return &Error{Kind: ErrorInvalidRequest}
	}
	if len(request.Messages) == 0 {
		return &Error{Kind: ErrorInvalidRequest}
	}
	for _, message := range request.Messages {
		switch message.Role {
		case llm.RoleSystem, llm.RoleUser, llm.RoleAssistant:
		default:
			return &Error{Kind: ErrorInvalidRequest}
		}
		if strings.TrimSpace(message.Content) == "" {
			return &Error{Kind: ErrorInvalidRequest}
		}
	}
	return nil
}

func classifyProviderError(provider string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return &Error{Kind: ErrorCanceled, Provider: provider}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Kind: ErrorTimeout, Provider: provider}
	}

	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return &Error{
			Kind:       ErrorUpstream,
			Provider:   provider,
			StatusCode: apiErr.StatusCode,
			RequestID:  apiErr.RequestID,
		}
	}
	return &Error{Kind: ErrorNetwork, Provider: provider}
}
