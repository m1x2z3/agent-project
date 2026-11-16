package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-project/internal/config"
)

func TestClientChat(t *testing.T) {
	var received chatRequest
	client := NewClient(config.Config{
		LLMBaseURL:     "https://example.test/v1",
		LLMAPIKey:      "test-key",
		LLMModel:       "test-model",
		LLMTemperature: 0.4,
		LLMTimeout:     time.Second,
	}, slog.Default())
	client.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if r.URL.Path != "/v1/chat/completions" {
				t.Errorf("path = %s, want /v1/chat/completions", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
				t.Errorf("authorization = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Fatalf("decode request: %v", err)
			}

			header := make(http.Header)
			header.Set("Content-Type", "application/json")
			header.Set("X-Request-ID", "req-123")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     header,
				Body: io.NopCloser(strings.NewReader(`{
					"id":"chatcmpl-1",
					"model":"test-model",
					"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
					"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
				}`)),
				Request: r,
			}, nil
		}),
	}

	result, err := client.Chat(context.Background(), []Message{
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if received.Model != "test-model" || received.Temperature != 0.4 {
		t.Fatalf("unexpected request: %+v", received)
	}
	if len(received.Messages) != 1 || received.Messages[0].Content != "hi" {
		t.Fatalf("unexpected messages: %+v", received.Messages)
	}
	if result.Choices[0].Message.Content != "hello" {
		t.Fatalf("content = %q", result.Choices[0].Message.Content)
	}
	if result.Usage.TotalTokens != 4 {
		t.Fatalf("total tokens = %d", result.Usage.TotalTokens)
	}
}

func TestClientChatReturnsAPIError(t *testing.T) {
	client := NewClient(config.Config{
		LLMBaseURL: "https://example.test",
		LLMAPIKey:  "bad-key",
		LLMModel:   "test-model",
	}, nil)
	client.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Set("Content-Type", "application/json")
			header.Set("X-Request-ID", "req-error")
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid api key","type":"authentication_error","code":"invalid_api_key"}}`)),
				Request:    r,
			}, nil
		}),
	}

	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Message != "invalid api key" {
		t.Fatalf("unexpected api error: %+v", apiErr)
	}
	if apiErr.RequestID != "req-error" {
		t.Fatalf("request id = %q", apiErr.RequestID)
	}
}

func TestClientChatRejectsMissingConfiguration(t *testing.T) {
	client := NewClient(config.Config{LLMBaseURL: "http://localhost"}, nil)

	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("error = %v, want ErrMissingAPIKey", err)
	}

	client = NewClient(config.Config{
		LLMBaseURL: "http://localhost",
		LLMAPIKey:  "test-key",
	}, nil)
	_, err = client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrMissingModel) {
		t.Fatalf("error = %v, want ErrMissingModel", err)
	}
}

func TestClientChatRejectsEmptyMessages(t *testing.T) {
	client := NewClient(config.Config{
		LLMBaseURL: "http://localhost",
		LLMAPIKey:  "test-key",
		LLMModel:   "test-model",
	}, nil)

	_, err := client.Chat(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "at least one message") {
		t.Fatalf("error = %v, want empty message validation error", err)
	}
}

func TestClientChatHonorsContextCancellation(t *testing.T) {
	client := NewClient(config.Config{
		LLMBaseURL: "http://localhost",
		LLMAPIKey:  "test-key",
		LLMModel:   "test-model",
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Chat(ctx, []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "canceled before request") {
		t.Fatalf("error = %v, want pre-request cancellation error", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
