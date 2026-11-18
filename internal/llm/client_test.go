package llm

import (
	"bytes"
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
	var logs bytes.Buffer
	client := NewClient(config.Config{
		LLMBaseURL: "https://example.test",
		LLMAPIKey:  "bad-key",
		LLMModel:   "test-model",
	}, slog.New(slog.NewTextHandler(&logs, nil)))
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
	if strings.Contains(logs.String(), "invalid api key") {
		t.Fatalf("log contains upstream error message: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "error_type=api_error") {
		t.Fatalf("log does not contain structured error type: %q", logs.String())
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

func TestClientChatRejectsOversizedResponse(t *testing.T) {
	client := NewClient(config.Config{
		LLMBaseURL: "http://example.test",
		LLMAPIKey:  "test-key",
		LLMModel:   "test-model",
	}, nil)
	client.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", maxResponseBodySize+1))),
				Request:    r,
			}, nil
		}),
	}

	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error = %v, want ErrResponseTooLarge", err)
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

func TestClientChatStreamParsesSSE(t *testing.T) {
	var received chatRequest
	var acceptHeader string
	body := strings.Join([]string{
		`data: {"id":"chatcmpl-stream","model":"test-model","choices":[{"index":0,`,
		`data: "delta":{"role":"assistant","content":"你"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-stream","model":"test-model","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl-stream","model":"test-model","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"",
		`data: [DONE]`,
	}, "\n")

	client := NewClient(config.Config{
		LLMBaseURL: "https://example.test/v1",
		LLMAPIKey:  "test-key",
		LLMModel:   "test-model",
	}, nil)
	client.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			acceptHeader = r.Header.Get("Accept")
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Fatalf("decode stream request: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		}),
	}

	stream, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	defer stream.Close()

	if !received.Stream || acceptHeader != "text/event-stream" {
		t.Fatalf("stream request = %+v, accept = %q", received, acceptHeader)
	}

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	if first.Sequence != 1 || first.ID != "chatcmpl-stream" || first.Choices[0].Delta.Content != "你" {
		t.Fatalf("unexpected first chunk: %+v", first)
	}
	if first.SequenceFromUpstream {
		t.Fatal("first sequence should be client-generated")
	}
	if first.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", first.Choices[0].Delta.Role)
	}

	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("second Recv() error = %v", err)
	}
	if second.Sequence != 2 || second.Choices[0].Delta.Content != "好" {
		t.Fatalf("unexpected second chunk: %+v", second)
	}
	if second.Choices[0].FinishReason == nil || *second.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish reason = %v, want stop", second.Choices[0].FinishReason)
	}

	third, err := stream.Recv()
	if err != nil {
		t.Fatalf("usage Recv() error = %v", err)
	}
	if third.Sequence != 3 || third.Usage == nil || third.Usage.TotalTokens != 5 {
		t.Fatalf("unexpected usage chunk: %+v", third)
	}

	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("done Recv() error = %v, want io.EOF", err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("repeated done Recv() error = %v, want io.EOF", err)
	}
}

func TestClientChatStreamParsesUpstreamSequence(t *testing.T) {
	body := strings.Join([]string{
		`data: {"sequence":7,"id":"chatcmpl-stream","choices":[{"index":0,"delta":{"content":"one"}}]}`,
		"",
		`data: {"sequence":8,"id":"chatcmpl-stream","choices":[{"index":0,"delta":{"content":"two"}}]}`,
		"",
		`data: [DONE]`,
	}, "\n")
	client := NewClient(config.Config{
		LLMBaseURL: "https://example.test",
		LLMAPIKey:  "test-key",
		LLMModel:   "test-model",
	}, nil)
	client.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		}),
	}

	stream, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	defer stream.Close()

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("second Recv() error = %v", err)
	}
	if first.Sequence != 7 || second.Sequence != 8 ||
		!first.SequenceFromUpstream || !second.SequenceFromUpstream {
		t.Fatalf("unexpected upstream sequence metadata: first=%+v second=%+v", first, second)
	}
}

func TestClientChatStreamMapsAPIErrors(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			var logs bytes.Buffer
			client := NewClient(config.Config{
				LLMBaseURL: "https://example.test",
				LLMAPIKey:  "test-key",
				LLMModel:   "test-model",
			}, slog.New(slog.NewTextHandler(&logs, nil)))
			client.httpClient = &http.Client{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					header := make(http.Header)
					header.Set("X-Request-ID", "upstream-request")
					return &http.Response{
						StatusCode: statusCode,
						Header:     header,
						Body: io.NopCloser(strings.NewReader(
							`{"error":{"message":"upstream failed","type":"provider_error","code":"provider_failure"}}`,
						)),
						Request: r,
					}, nil
				}),
			}

			_, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error type = %T, want *APIError", err)
			}
			if apiErr.StatusCode != statusCode || apiErr.RequestID != "upstream-request" {
				t.Fatalf("unexpected API error: %+v", apiErr)
			}
			if strings.Contains(logs.String(), "upstream failed") {
				t.Fatalf("log contains upstream error message: %q", logs.String())
			}
			if !strings.Contains(logs.String(), "error_type=api_error") {
				t.Fatalf("log does not contain structured error type: %q", logs.String())
			}
		})
	}
}

func TestClientChatStreamRejectsInvalidSSE(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "invalid json",
			body: "data: {not-json}\n\n",
		},
		{
			name: "unsupported field",
			body: "unknown: value\n\n",
		},
		{
			name: "empty chunk",
			body: "data: {\"id\":\"x\",\"choices\":[]}\n\n",
		},
		{
			name: "event too large",
			body: "data: " + strings.Repeat("x", maxStreamEventSize),
		},
		{
			name: "unexpected eof",
			body: "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := NewClient(config.Config{
				LLMBaseURL: "https://example.test",
				LLMAPIKey:  "test-key",
				LLMModel:   "test-model",
			}, nil)
			client.httpClient = &http.Client{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(test.body)),
						Request:    r,
					}, nil
				}),
			}

			stream, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}})
			if err != nil {
				t.Fatalf("ChatStream() error = %v", err)
			}
			defer stream.Close()

			_, err = stream.Recv()
			if test.name == "unexpected eof" {
				if err != nil {
					t.Fatalf("first Recv() error = %v, want nil for complete data event", err)
				}
				_, err = stream.Recv()
			}
			if !errors.Is(err, ErrInvalidStream) {
				t.Fatalf("Recv() error = %v, want ErrInvalidStream", err)
			}
		})
	}
}

func TestClientChatStreamCloseIsIdempotent(t *testing.T) {
	body := &countingReadCloser{Reader: strings.NewReader("data: [DONE]\n\n")}
	client := NewClient(config.Config{
		LLMBaseURL: "https://example.test",
		LLMAPIKey:  "test-key",
		LLMModel:   "test-model",
	}, nil)
	client.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
				Request:    r,
			}, nil
		}),
	}

	stream, err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if body.closeCount != 1 {
		t.Fatalf("close count = %d, want 1", body.closeCount)
	}
}

func TestClientChatStreamHonorsContextCancellation(t *testing.T) {
	client := NewClient(config.Config{
		LLMBaseURL: "https://example.test",
		LLMAPIKey:  "test-key",
		LLMModel:   "test-model",
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.ChatStream(ctx, []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "canceled before request") {
		t.Fatalf("error = %v, want pre-request cancellation error", err)
	}
}

type countingReadCloser struct {
	io.Reader
	closeCount int
}

func (r *countingReadCloser) Close() error {
	r.closeCount++
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
