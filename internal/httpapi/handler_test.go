package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-project/internal/llm"
)

// TestNewHandlerRandomSource 验证系统默认值、显式注入和旧构造方式兼容。
func TestNewHandlerRandomSource(t *testing.T) {
	if handler := NewHandler(nil, nil); handler.random != rand.Reader {
		t.Fatal("NewHandler() did not default to crypto/rand.Reader")
	}
	injected := bytes.NewReader([]byte{0x01})
	if handler := NewHandler(nil, nil, injected); handler.random != injected {
		t.Fatal("NewHandler() did not retain injected random source")
	}
	if handler := NewHandler(nil, nil, nil, injected); handler.random != injected {
		t.Fatal("NewHandler() did not select the first non-nil random source")
	}
}

// TestNewRequestIDUsesInjectedRandomSource 验证明确字节序列、无参数兼容和失败回退。
func TestNewRequestIDUsesInjectedRandomSource(t *testing.T) {
	randomBytes := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}
	const want = "req-0001020304050607"
	if got := newRequestID(bytes.NewReader(randomBytes)); got != want {
		t.Fatalf("newRequestID(injected) = %q, want %q", got, want)
	}
	if got := newRequestID(bytes.NewReader(randomBytes)); got != want {
		t.Fatalf("second newRequestID(injected) = %q, want %q", got, want)
	}
	if got := newRequestID(); !strings.HasPrefix(got, "req-") {
		t.Fatalf("newRequestID() = %q, want existing req- prefix", got)
	}
	if got := newRequestID(bytes.NewReader([]byte{0x01})); !strings.HasPrefix(got, "req-") || got == want {
		t.Fatalf("newRequestID(short source) = %q, want timestamp fallback", got)
	}
}

// TestHandlerRequestIDUsesInjectedRandomSource 验证 HTTP Header 使用注入字节且接口路径不变。
func TestHandlerRequestIDUsesInjectedRandomSource(t *testing.T) {
	randomBytes := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}
	const want = "req-0001020304050607"
	serve := func() string {
		handler := NewHandler(nil, nil, bytes.NewReader(randomBytes))
		request := httptest.NewRequest(http.MethodGet, "/health", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET /health status = %d, want 200", response.Code)
		}
		return response.Header().Get("X-Request-ID")
	}
	if got := serve(); got != want {
		t.Fatalf("X-Request-ID = %q, want %q", got, want)
	}
	if got := serve(); got != want {
		t.Fatalf("repeated X-Request-ID = %q, want deterministic %q", got, want)
	}
}

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

func TestHandlerRejectsUnsupportedMethods(t *testing.T) {
	handler := NewHandler(&fakeChatClient{}, nil)
	tests := []struct {
		path        string
		method      string
		allowMethod string
	}{
		{path: "/health", method: http.MethodPost, allowMethod: http.MethodGet},
		{path: "/chat", method: http.MethodGet, allowMethod: http.MethodPost},
		{path: "/chat/stream", method: http.MethodGet, allowMethod: http.MethodPost},
	}
	for _, test := range tests {
		test := test
		t.Run(test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			assertErrorResponse(t, response, http.StatusMethodNotAllowed, "method_not_allowed", "")
			if got := response.Header().Get("Allow"); got != test.allowMethod {
				t.Fatalf("Allow = %q, want %q", got, test.allowMethod)
			}
		})
	}
}

func TestHandlerWithNilClientReturnsInternalError(t *testing.T) {
	handler := NewHandler(nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/chat",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	))

	assertErrorResponse(t, response, http.StatusInternalServerError, "internal_error", "")
}

func TestHandlerHealthLogsTotalDuration(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := NewHandler(&fakeChatClient{}, logger)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))

	logLine := logs.String()
	assertLogFields(t, logLine,
		"msg=\"http request completed\"",
		"request_id=req-",
		"method=GET",
		"path=/health",
		"status=200",
		"duration_ms=",
		"total_duration_ms=",
	)
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

func TestHandlerChatLogsDurationsAndRedactsContent(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	client := &fakeChatClient{
		response: llm.ChatResponse{
			ID:    "chatcmpl-1",
			Model: "test-model",
			Choices: []llm.Choice{{
				Message:      llm.Message{Role: llm.RoleAssistant, Content: "secret answer"},
				FinishReason: "stop",
			}},
			Usage: llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		},
	}
	handler := NewHandler(client, logger)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(
		http.MethodPost,
		"/chat",
		strings.NewReader(`{"messages":[{"role":"user","content":"secret prompt"}]}`),
	))

	logLine := logs.String()
	assertLogFields(t, logLine,
		"msg=\"http request parsed\"",
		"parse_duration_ms=",
		"msg=\"model call observed\"",
		"model_duration_ms=",
		"msg=\"http request completed\"",
		"method=POST",
		"path=/chat",
		"status=200",
		"total_duration_ms=",
	)
	if strings.Contains(logLine, "secret prompt") || strings.Contains(logLine, "secret answer") || strings.Contains(logLine, "test-key") {
		t.Fatalf("log contains sensitive content: %q", logLine)
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

func TestHandlerChatStreamSuccess(t *testing.T) {
	finishReason := "stop"
	client := &fakeStreamingClient{
		stream: &fakeStream{chunks: []llm.StreamChunk{
			{
				Sequence: 1,
				ID:       "chatcmpl-stream",
				Model:    "deepseek-flash",
				Choices: []llm.StreamChoice{{
					Index: 0,
					Delta: llm.Message{Role: "assistant", Content: "你"},
				}},
			},
			{
				Sequence: 2,
				ID:       "chatcmpl-stream",
				Model:    "deepseek-flash",
				Choices: []llm.StreamChoice{{
					Index:        0,
					Delta:        llm.Message{Content: "好"},
					FinishReason: &finishReason,
				}},
			},
			{
				Sequence: 3,
				ID:       "chatcmpl-stream",
				Model:    "deepseek-flash",
				Usage:    &llm.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
			},
		}},
	}
	request := httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(
		`{"messages":[{"role":"user","content":"你好"}]}`,
	))
	response := httptest.NewRecorder()
	handler := NewHandler(client, nil)
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache control = %q", got)
	}
	if got := response.Header().Get("Connection"); got != "keep-alive" {
		t.Fatalf("connection = %q", got)
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID is empty")
	}

	events := parseSSEEvents(t, response.Body.String())
	if len(events) != 3 {
		t.Fatalf("event count = %d, want 3; body=%s", len(events), response.Body.String())
	}
	if events[0].Name != "message.delta" || events[1].Name != "message.delta" || events[2].Name != "message.done" {
		t.Fatalf("event names = %+v", events)
	}

	var first streamDeltaEvent
	if err := json.Unmarshal(events[0].Data, &first); err != nil {
		t.Fatalf("decode first event: %v", err)
	}
	if first.Sequence != 1 || first.Delta.Content != "你" {
		t.Fatalf("first delta = %+v", first)
	}

	var done streamDoneEvent
	if err := json.Unmarshal(events[2].Data, &done); err != nil {
		t.Fatalf("decode done event: %v", err)
	}
	if done.FinishReason == nil || *done.FinishReason != "stop" {
		t.Fatalf("finish reason = %v", done.FinishReason)
	}
	if done.Usage == nil || done.Usage.TotalTokens != 12 {
		t.Fatalf("usage = %+v", done.Usage)
	}
	if len(client.messages) != 1 || client.messages[0].Content != "你好" {
		t.Fatalf("messages = %+v", client.messages)
	}
}

func TestHandlerChatStreamLogsFirstTokenAndTotalDuration(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	finishReason := "stop"
	client := &fakeStreamingClient{
		stream: &fakeStream{
			chunks: []llm.StreamChunk{
				{
					Sequence: 1,
					ID:       "chatcmpl-stream",
					Model:    "test-model",
					Choices: []llm.StreamChoice{{
						Index: 0,
						Delta: llm.Message{Role: llm.RoleAssistant, Content: "private answer"},
					}},
				},
				{
					Sequence: 2,
					ID:       "chatcmpl-stream",
					Model:    "test-model",
					Choices: []llm.StreamChoice{{
						Index:        0,
						Delta:        llm.Message{Content: " done"},
						FinishReason: &finishReason,
					}},
				},
			},
		},
	}
	handler := NewHandler(client, logger)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"private prompt"}]}`),
	))

	logLine := logs.String()
	assertLogFields(t, logLine,
		"msg=\"http request completed\"",
		"method=POST",
		"path=/chat/stream",
		"status=200",
		"total_duration_ms=",
	)
	assertLogFields(t, logLine,
		"msg=\"model stream observed\"",
		"path=/chat/stream",
		"first_token_duration_ms=",
		"finish_reason=stop",
	)
	if strings.Contains(logLine, "private prompt") || strings.Contains(logLine, "private answer") {
		t.Fatalf("log contains sensitive content: %q", logLine)
	}
}

func TestHandlerChatStreamLogsErrorWithoutSuccessFields(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := NewHandler(&fakeStreamingClient{
		streamErr: context.DeadlineExceeded,
	}, logger)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"timeout test"}]}`),
	))

	logLine := logs.String()
	assertLogFields(t, logLine,
		"msg=\"model stream observed\"",
		"path=/chat/stream",
		"model_duration_ms=",
		"error_type=timeout",
	)
	if strings.Contains(logLine, "finish_reason=") || strings.Contains(logLine, "first_token_duration_ms=") {
		t.Fatalf("error log contains success-only fields: %q", logLine)
	}
}

func TestObservedChatStreamCloseIsIdempotent(t *testing.T) {
	closeErr := errors.New("close failed")
	stream := &fakeStream{closeErr: closeErr}
	observed := &observedChatStream{
		next:      stream,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		ctx:       context.Background(),
		startedAt: time.Now(),
	}

	firstErr := observed.Close()
	secondErr := observed.Close()

	if !errors.Is(firstErr, closeErr) || !errors.Is(secondErr, closeErr) {
		t.Fatalf("close errors = %v, %v; want %v", firstErr, secondErr, closeErr)
	}
	if firstErr != secondErr {
		t.Fatalf("close error pointers differ: first=%p second=%p", firstErr, secondErr)
	}
	if stream.closeCall != 1 {
		t.Fatalf("close count = %d, want 1", stream.closeCall)
	}
}

func TestHandlerChatStreamOmitsUsageWhenUnavailable(t *testing.T) {
	client := &fakeStreamingClient{
		stream: &fakeStream{chunks: []llm.StreamChunk{{
			Sequence: 1,
			ID:       "chatcmpl-stream",
			Model:    "deepseek-flash",
			Choices: []llm.StreamChoice{{
				Index: 0,
				Delta: llm.Message{Content: "hello"},
			}},
		}}},
	}
	response := httptest.NewRecorder()
	handler := NewHandler(client, nil)
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	))

	events := parseSSEEvents(t, response.Body.String())
	var done map[string]json.RawMessage
	if err := json.Unmarshal(events[len(events)-1].Data, &done); err != nil {
		t.Fatalf("decode done event: %v", err)
	}
	if _, ok := done["usage"]; ok {
		t.Fatal("done event contains usage without upstream usage")
	}
}

func TestHandlerChatStreamRejectsEmptyOrIncompleteUpstreamResponse(t *testing.T) {
	tests := []struct {
		name   string
		chunks []llm.StreamChunk
	}{
		{
			name: "done without delta",
		},
		{
			name: "delta without metadata",
			chunks: []llm.StreamChunk{{
				Choices: []llm.StreamChoice{{
					Delta: llm.Message{Role: llm.RoleAssistant, Content: "partial"},
				}},
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(&fakeStreamingClient{
				stream: &fakeStream{chunks: test.chunks},
			}, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(
				http.MethodPost,
				"/chat/stream",
				strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
			))

			events := parseSSEEvents(t, response.Body.String())
			if len(events) == 0 || events[len(events)-1].Name != "error" {
				t.Fatalf("events = %+v; want final error event", events)
			}
			for _, event := range events {
				if event.Name == "message.done" {
					t.Fatalf("received success event for incomplete stream: %+v", events)
				}
			}
			var streamErr streamErrorEvent
			if err := json.Unmarshal(events[len(events)-1].Data, &streamErr); err != nil {
				t.Fatalf("decode stream error: %v", err)
			}
			if streamErr.Code != "model_error" {
				t.Fatalf("error code = %q, want model_error", streamErr.Code)
			}
		})
	}
}

func TestHandlerChatStreamRejectsInvalidRequests(t *testing.T) {
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
			handler := NewHandler(&fakeStreamingClient{}, nil)
			request := httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertErrorResponse(t, response, http.StatusBadRequest, "invalid_request", test.message)
		})
	}
}

func TestHandlerChatStreamRejectsOversizedBody(t *testing.T) {
	handler := NewHandler(&fakeStreamingClient{}, nil)
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", maxRequestBodySize) + `"}` + "}"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(body)))

	assertErrorResponse(t, response, http.StatusBadRequest, "invalid_request", "request body too large")
}

func TestHandlerChatStreamMapsPreHeaderErrors(t *testing.T) {
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(&fakeStreamingClient{streamErr: test.err}, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(
				http.MethodPost,
				"/chat/stream",
				strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
			))
			assertErrorResponse(t, response, test.statusCode, test.code, "")
		})
	}
}

func TestHandlerChatStreamWritesPostHeaderErrorEvent(t *testing.T) {
	client := &fakeStreamingClient{
		stream: &fakeStream{
			chunks: []llm.StreamChunk{{
				Sequence: 1,
				ID:       "chatcmpl-stream",
				Model:    "deepseek-flash",
				Choices: []llm.StreamChoice{{
					Index: 0,
					Delta: llm.Message{Content: "partial"},
				}},
			}},
			errs: []error{nil, &llm.APIError{StatusCode: http.StatusBadGateway}},
		},
	}
	response := httptest.NewRecorder()
	handler := NewHandler(client, nil)
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	))

	events := parseSSEEvents(t, response.Body.String())
	if len(events) != 2 || events[0].Name != "message.delta" || events[1].Name != "error" {
		t.Fatalf("events = %+v", events)
	}
	var event streamErrorEvent
	if err := json.Unmarshal(events[1].Data, &event); err != nil {
		t.Fatalf("decode error event: %v", err)
	}
	if event.Code != "model_error" || event.RequestID == "" {
		t.Fatalf("error event = %+v", event)
	}
}

func TestHandlerChatStreamRejectsOutOfOrderSequence(t *testing.T) {
	client := &fakeStreamingClient{
		stream: &fakeStream{chunks: []llm.StreamChunk{
			{Sequence: 1, SequenceFromUpstream: true, ID: "chatcmpl-sequence", Model: "test-model", Choices: []llm.StreamChoice{{Delta: llm.Message{Content: "one"}}}},
			{Sequence: 3, SequenceFromUpstream: true, ID: "chatcmpl-sequence", Model: "test-model", Choices: []llm.StreamChoice{{Delta: llm.Message{Content: "three"}}}},
		}},
	}
	response := httptest.NewRecorder()
	handler := NewHandler(client, nil)
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	))

	events := parseSSEEvents(t, response.Body.String())
	if len(events) != 2 || events[1].Name != "error" {
		t.Fatalf("events = %+v", events)
	}
	var event streamErrorEvent
	if err := json.Unmarshal(events[1].Data, &event); err != nil {
		t.Fatalf("decode error event: %v", err)
	}
	if event.Code != "model_error" || event.Message != "stream sequence is out of order" {
		t.Fatalf("error event = %+v", event)
	}
}

func TestHandlerChatStreamRejectsDuplicateSequence(t *testing.T) {
	client := &fakeStreamingClient{
		stream: &fakeStream{chunks: []llm.StreamChunk{
			{Sequence: 1, SequenceFromUpstream: true, ID: "chatcmpl-sequence", Model: "test-model", Choices: []llm.StreamChoice{{Delta: llm.Message{Content: "one"}}}},
			{Sequence: 1, SequenceFromUpstream: true, ID: "chatcmpl-sequence", Model: "test-model", Choices: []llm.StreamChoice{{Delta: llm.Message{Content: "duplicate"}}}},
		}},
	}
	response := httptest.NewRecorder()
	handler := NewHandler(client, nil)
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	))

	events := parseSSEEvents(t, response.Body.String())
	if len(events) != 2 || events[1].Name != "error" {
		t.Fatalf("events = %+v", events)
	}
	var event streamErrorEvent
	if err := json.Unmarshal(events[1].Data, &event); err != nil {
		t.Fatalf("decode error event: %v", err)
	}
	if event.Code != "model_error" || event.Message != "stream sequence is out of order" {
		t.Fatalf("error event = %+v", event)
	}
}

func TestHandlerChatStreamStopsOnCanceledContext(t *testing.T) {
	handler := NewHandler(&fakeStreamingClient{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := &trackingResponseWriter{header: make(http.Header)}
	request := httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	).WithContext(ctx)

	handler.ServeHTTP(response, request)
	if response.wrote {
		t.Fatal("handler wrote a response after request context cancellation")
	}
}

func TestHandlerChatStreamStopsAfterHeaderWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeStreamingClient{
		stream: &fakeStream{
			recvFunc: func() (llm.StreamChunk, error) {
				cancel()
				return llm.StreamChunk{}, context.Canceled
			},
		},
	}
	handler := NewHandler(client, nil)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	).WithContext(ctx)

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("body = %q, want no event after cancellation", response.Body.String())
	}
}

func TestHandlerChatStreamRequiresStreamingClient(t *testing.T) {
	handler := NewHandler(&fakeChatClient{}, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	))

	assertErrorResponse(t, response, http.StatusInternalServerError, "internal_error", "streaming model client")
}

func TestHandlerChatStreamRequiresFlusher(t *testing.T) {
	handler := NewHandler(&fakeStreamingClient{}, nil)
	response := &nonFlushingResponseWriter{header: make(http.Header)}
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/chat/stream",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	))

	if response.statusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.statusCode)
	}
	if !strings.Contains(response.body.String(), "streaming response is unsupported") {
		t.Fatalf("body = %q", response.body.String())
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

func assertLogFields(t *testing.T, logLine string, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if !strings.Contains(logLine, field) {
			t.Errorf("log = %q, missing field %q", logLine, field)
		}
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

type fakeStreamingClient struct {
	messages  []llm.Message
	stream    llm.ChatStream
	streamErr error
}

func (f *fakeStreamingClient) Chat(_ context.Context, messages []llm.Message) (llm.ChatResponse, error) {
	f.messages = append(f.messages, messages...)
	return llm.ChatResponse{}, nil
}

func (f *fakeStreamingClient) ChatStream(_ context.Context, messages []llm.Message) (llm.ChatStream, error) {
	f.messages = append(f.messages, messages...)
	return f.stream, f.streamErr
}

type fakeStream struct {
	chunks    []llm.StreamChunk
	errs      []error
	index     int
	closeCall int
	closeErr  error
	recvFunc  func() (llm.StreamChunk, error)
}

func (f *fakeStream) Recv() (llm.StreamChunk, error) {
	if f.recvFunc != nil {
		return f.recvFunc()
	}
	if f.index >= len(f.chunks) {
		if f.index < len(f.errs) && f.errs[f.index] != nil {
			err := f.errs[f.index]
			f.index++
			return llm.StreamChunk{}, err
		}
		return llm.StreamChunk{}, io.EOF
	}
	chunk := f.chunks[f.index]
	var err error
	if f.index < len(f.errs) {
		err = f.errs[f.index]
	}
	f.index++
	if err != nil {
		return llm.StreamChunk{}, err
	}
	return chunk, nil
}

func (f *fakeStream) Close() error {
	f.closeCall++
	return f.closeErr
}

type sseTestEvent struct {
	Name string
	Data json.RawMessage
}

func parseSSEEvents(t *testing.T, body string) []sseTestEvent {
	t.Helper()
	var events []sseTestEvent
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if block == "" {
			continue
		}
		var event sseTestEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				event.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			}
		}
		events = append(events, event)
	}
	return events
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

type nonFlushingResponseWriter struct {
	header     http.Header
	statusCode int
	body       bytes.Buffer
}

func (w *nonFlushingResponseWriter) Header() http.Header {
	return w.header
}

func (w *nonFlushingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *nonFlushingResponseWriter) Write(data []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	return w.body.Write(data)
}
