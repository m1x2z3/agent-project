package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-project/internal/llm"
)

func TestHandlerHealth(t *testing.T) {
	handler := NewHandler(&fakeChatClient{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var body healthResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q, want ok", body.Status)
	}
}

func TestHandlerChatSuccess(t *testing.T) {
	client := fakeChatClient{
		response: llm.ChatResponse{
			ID:    "chatcmpl-1",
			Model: "deepseek-flash",
			Choices: []llm.Choice{{
				Message: llm.Message{Role: "assistant", Content: "你好"},
			}},
			Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	}
	handler := NewHandler(&client, nil)

	request := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"messages":[{"role":"user","content":"你好"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var body chatResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.ID != "chatcmpl-1" || body.Model != "deepseek-flash" {
		t.Fatalf("unexpected response metadata: %+v", body)
	}
	if body.Message.Content != "你好" || body.Usage.TotalTokens != 15 {
		t.Fatalf("unexpected response body: %+v", body)
	}
	if len(client.messages) != 1 || client.messages[0].Content != "你好" {
		t.Fatalf("unexpected client messages: %+v", client.messages)
	}
}

func TestHandlerChatRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		message string
	}{
		{name: "malformed JSON", body: `{"messages":[}`, message: "invalid JSON body"},
		{name: "empty messages", body: `{"messages":[]}`, message: "messages must not be empty"},
		{name: "invalid role", body: `{"messages":[{"role":"tool","content":"hi"}]}`, message: "messages[0].role"},
		{name: "empty content", body: `{"messages":[{"role":"user","content":"  "}]}`, message: "messages[0].content"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(&fakeChatClient{}, nil)
			request := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			assertErrorResponse(t, response, http.StatusBadRequest, "invalid_request", test.message)
		})
	}
}

func TestHandlerChatRejectsTooManyMessages(t *testing.T) {
	messages := make([]chatMessage, maxMessageCount+1)
	for i := range messages {
		messages[i] = chatMessage{Role: "user", Content: "hi"}
	}
	body, err := json.Marshal(chatRequest{Messages: messages})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	handler := NewHandler(&fakeChatClient{}, nil)
	request := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	assertErrorResponse(t, response, http.StatusBadRequest, "invalid_request", "more than 32")
}

func TestHandlerChatRejectsOversizedBody(t *testing.T) {
	handler := NewHandler(&fakeChatClient{}, nil)
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", maxRequestBodySize) + `"}` + "}"
	request := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	assertErrorResponse(t, response, http.StatusBadRequest, "invalid_request", "request body too large")
}

func TestHandlerChatMapsModelErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		statusCode int
		code       string
	}{
		{
			name:       "api error",
			err:        &llm.APIError{StatusCode: http.StatusBadGateway, Message: "upstream failed"},
			statusCode: http.StatusBadGateway,
			code:       "model_error",
		},
		{
			name:       "timeout",
			err:        context.DeadlineExceeded,
			statusCode: http.StatusGatewayTimeout,
			code:       "model_timeout",
		},
		{
			name:       "internal error",
			err:        errors.New("unexpected failure"),
			statusCode: http.StatusInternalServerError,
			code:       "internal_error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(&fakeChatClient{err: test.err}, nil)
			request := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			assertErrorResponse(t, response, test.statusCode, test.code, "")
		})
	}
}

func TestHandlerChatStopsWhenContextCanceled(t *testing.T) {
	handler := NewHandler(&fakeChatClient{err: context.Canceled}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	response := &trackingResponseWriter{header: make(http.Header)}

	handler.ServeHTTP(response, request)

	if response.wrote {
		t.Fatal("handler wrote a response after request context cancellation")
	}
}

func assertErrorResponse(t *testing.T, response *httptest.ResponseRecorder, statusCode int, code, message string) {
	t.Helper()
	if response.Code != statusCode {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, statusCode, response.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Code != code {
		t.Fatalf("code = %q, want %q", body.Code, code)
	}
	if message != "" && !strings.Contains(body.Message, message) {
		t.Fatalf("message = %q, want substring %q", body.Message, message)
	}
	if body.RequestID == "" {
		t.Fatal("request_id is empty")
	}
}

type fakeChatClient struct {
	messages []llm.Message
	response llm.ChatResponse
	err      error
}

func (f *fakeChatClient) Chat(_ context.Context, messages []llm.Message) (llm.ChatResponse, error) {
	f.messages = append(f.messages, messages...)
	return f.response, f.err
}

type trackingResponseWriter struct {
	header http.Header
	wrote  bool
}

func (w *trackingResponseWriter) Header() http.Header {
	return w.header
}

func (w *trackingResponseWriter) WriteHeader(int) {
	w.wrote = true
}

func (w *trackingResponseWriter) Write([]byte) (int, error) {
	w.wrote = true
	return 0, nil
}
