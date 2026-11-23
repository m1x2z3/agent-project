package modelgateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-project/internal/config"
	"agent-project/internal/llm"
)

func TestOpenAICompatibleProviderChat(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider(config.ProviderConfig{
		Name:        "deepseek",
		BaseURL:     "https://deepseek.example/v1",
		APIKey:      "test-key",
		Model:       "deepseek-chat",
		Temperature: 0.4,
		MaxTokens:   128,
		Timeout:     time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("NewOpenAICompatibleProvider() error = %v", err)
	}
	provider.client = &fakeLLMClient{
		response: llm.ChatResponse{
			ID:    "chatcmpl-1",
			Model: "deepseek-chat",
			Choices: []llm.Choice{{
				Message:      llm.Message{Role: "assistant", Content: "answer"},
				FinishReason: "stop",
			}},
			Usage: llm.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
		},
	}

	result, err := provider.Chat(context.Background(), ChatRequest{
		Task: "chat",
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got := provider.client.(*fakeLLMClient).messages; len(got) != 1 || got[0].Content != "hello" {
		t.Fatalf("unexpected messages: %+v", got)
	}
	if result.Provider != "deepseek" ||
		result.Model != "deepseek-chat" ||
		result.Message.Content != "answer" ||
		result.FinishReason != "stop" ||
		result.Usage == nil ||
		result.Usage.TotalTokens != 6 {
		t.Fatalf("unexpected gateway response: %+v", result)
	}
}

func TestOpenAICompatibleProviderAppliesPerRequestOptions(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider(validProviderConfig("deepseek", "https://deepseek.example/v1"), nil)
	if err != nil {
		t.Fatalf("NewOpenAICompatibleProvider() error = %v", err)
	}
	client := &recordingLLMClient{
		response: llm.ChatResponse{
			Model: "route-model",
			Choices: []llm.Choice{{
				Message:      llm.Message{Role: "assistant", Content: "answer"},
				FinishReason: "stop",
			}},
		},
	}
	provider.client = client
	temperature := 0.7
	maxTokens := 256

	_, err = provider.Chat(context.Background(), ChatRequest{
		Task:        "analysis",
		Model:       "route-model",
		Temperature: &temperature,
		MaxTokens:   &maxTokens,
		Timeout:     time.Second,
		Messages:    []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if client.options.Model != "route-model" ||
		client.options.Temperature == nil || *client.options.Temperature != temperature ||
		client.options.MaxTokens == nil || *client.options.MaxTokens != maxTokens {
		t.Fatalf("options = %+v, want per-request route options", client.options)
	}
}

func TestOpenAICompatibleProviderConvertsAPIErrorWithoutRawMessage(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider(validProviderConfig("kimi", "https://kimi.example/v1"), nil)
	if err != nil {
		t.Fatalf("NewOpenAICompatibleProvider() error = %v", err)
	}
	provider.client = &fakeLLMClient{
		err: &llm.APIError{
			StatusCode: http.StatusTooManyRequests,
			RequestID:  "upstream-123",
			Message:    "sensitive provider response",
		},
	}

	_, err = provider.Chat(context.Background(), validChatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if gatewayErr.Kind != ErrorUpstream ||
		gatewayErr.StatusCode != http.StatusTooManyRequests ||
		gatewayErr.RequestID != "upstream-123" {
		t.Fatalf("unexpected gateway error: %+v", gatewayErr)
	}
	if strings.Contains(err.Error(), "sensitive provider response") {
		t.Fatalf("gateway error exposed provider message: %q", err)
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("error = %v, want ErrUpstream", err)
	}
}

func TestOpenAICompatibleProviderClassifiesCancellationAndTimeout(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider(validProviderConfig("deepseek", "https://deepseek.example/v1"), nil)
	if err != nil {
		t.Fatalf("NewOpenAICompatibleProvider() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider.client = &fakeLLMClient{err: context.Canceled}
	_, err = provider.Chat(ctx, validChatRequest())
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCanceled) {
		t.Fatalf("canceled error = %v, want context.Canceled and ErrCanceled", err)
	}
}

func TestRouterSelectionPriority(t *testing.T) {
	providers := []*fakeProvider{
		{name: "deepseek", health: ProviderHealth{Status: ProviderHealthy}},
		{name: "kimi", health: ProviderHealth{Status: ProviderHealthy}},
		{name: "local", health: ProviderHealth{Status: ProviderHealthy}},
	}
	router, err := NewRouter(
		[]ModelProvider{providers[0], providers[1], providers[2]},
		"deepseek",
		map[string]string{"chat": "kimi"},
	)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	selected, err := router.Select(ChatRequest{Task: "chat"})
	if err != nil || selected.Name() != "kimi" {
		t.Fatalf("task route selected %q, error = %v", selectedName(selected), err)
	}

	selected, err = router.Select(ChatRequest{Task: "chat", Provider: "local"})
	if err != nil || selected.Name() != "local" {
		t.Fatalf("explicit provider selected %q, error = %v", selectedName(selected), err)
	}

	selected, err = router.Select(ChatRequest{Task: "unknown"})
	if err != nil || selected.Name() != "deepseek" {
		t.Fatalf("default provider selected %q, error = %v", selectedName(selected), err)
	}
}

func TestRouterRejectsUnknownAndUnhealthyProviders(t *testing.T) {
	unhealthy := &fakeProvider{
		name:   "deepseek",
		health: ProviderHealth{Status: ProviderUnhealthy},
	}
	router, err := NewRouter([]ModelProvider{unhealthy}, "deepseek", nil)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	_, err = router.Chat(context.Background(), validChatRequest())
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("unhealthy provider error = %v, want ErrProviderUnavailable", err)
	}
	if unhealthy.calls != 0 {
		t.Fatalf("unhealthy provider calls = %d, want 0", unhealthy.calls)
	}

	_, err = router.Select(ChatRequest{Provider: "missing"})
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("unknown provider error = %v, want ErrProviderNotFound", err)
	}
}

func TestNewRouterFromConfigSupportsMultipleProviders(t *testing.T) {
	cfg := config.Config{
		LLMDefaultProvider: "deepseek",
		LLMProviders: map[string]config.ProviderConfig{
			"deepseek": validProviderConfig("deepseek", "https://deepseek.example/v1"),
			"kimi":     validProviderConfig("kimi", "https://kimi.example/v1"),
		},
		LLMRoutes: map[string]string{"analysis": "kimi"},
	}

	router, err := NewRouterFromConfig(cfg, nil)
	if err != nil {
		t.Fatalf("NewRouterFromConfig() error = %v", err)
	}
	provider, err := router.Select(ChatRequest{Task: "analysis"})
	if err != nil || provider.Name() != "kimi" {
		t.Fatalf("selected provider = %q, error = %v", selectedName(provider), err)
	}
}

func TestProviderHealthDoesNotCallUpstream(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider(validProviderConfig("deepseek", "https://deepseek.example/v1"), nil)
	if err != nil {
		t.Fatalf("NewOpenAICompatibleProvider() error = %v", err)
	}
	health := provider.Health(context.Background())
	if health.Status != ProviderConfigured {
		t.Fatalf("health status = %q, want configured", health.Status)
	}
}

func validProviderConfig(name, baseURL string) config.ProviderConfig {
	return config.ProviderConfig{
		Name:        name,
		BaseURL:     baseURL,
		APIKey:      "test-key",
		Model:       "test-model",
		Temperature: 0.2,
		Timeout:     time.Second,
	}
}

func validChatRequest() ChatRequest {
	return ChatRequest{
		Task:     "chat",
		Messages: []Message{{Role: "user", Content: "hello"}},
	}
}

type fakeProvider struct {
	name   string
	health ProviderHealth
	calls  int
}

type fakeLLMClient struct {
	response llm.ChatResponse
	err      error
	messages []llm.Message
}

func (f *fakeLLMClient) Chat(_ context.Context, messages []llm.Message) (llm.ChatResponse, error) {
	f.messages = append([]llm.Message(nil), messages...)
	return f.response, f.err
}

func (f *fakeLLMClient) ChatWithOptions(ctx context.Context, messages []llm.Message, _ llm.ChatOptions) (llm.ChatResponse, error) {
	return f.Chat(ctx, messages)
}

type recordingLLMClient struct {
	response llm.ChatResponse
	options  llm.ChatOptions
}

func (c *recordingLLMClient) ChatWithOptions(_ context.Context, _ []llm.Message, options llm.ChatOptions) (llm.ChatResponse, error) {
	c.options = options
	return c.response, nil
}

func (f *fakeProvider) Name() string {
	return f.name
}

func (f *fakeProvider) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	f.calls++
	return ChatResponse{Provider: f.name}, nil
}

func (f *fakeProvider) Health(context.Context) ProviderHealth {
	return f.health
}

func selectedName(provider ModelProvider) string {
	if provider == nil {
		return ""
	}
	return provider.Name()
}
