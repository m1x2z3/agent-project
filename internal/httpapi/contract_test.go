package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"agent-project/internal/llm"
)

func TestOpenAPIContractDocumentsImplementedRoutes(t *testing.T) {
	spec := readOpenAPISpec(t)
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI paths are missing or invalid")
	}

	expectedPaths := map[string]string{
		"/health":      "get",
		"/chat":        "post",
		"/chat/stream": "post",
	}
	for path, method := range expectedPaths {
		pathItem, ok := paths[path].(map[string]any)
		if !ok {
			t.Fatalf("OpenAPI path %q is missing", path)
		}
		if _, ok := pathItem[method].(map[string]any); !ok {
			t.Fatalf("OpenAPI method %s %s is missing", method, path)
		}
	}

	if len(paths) != len(expectedPaths) {
		t.Fatalf("OpenAPI path count = %d, want %d", len(paths), len(expectedPaths))
	}

	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI components are missing or invalid")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI schemas are missing or invalid")
	}
	for _, schemaName := range []string{
		"HealthResponse",
		"ChatRequest",
		"ChatResponse",
		"ErrorResponse",
		"StreamDeltaEvent",
		"StreamMessageDelta",
		"StreamDoneEvent",
		"StreamErrorEvent",
	} {
		if _, ok := schemas[schemaName]; !ok {
			t.Fatalf("OpenAPI schema %q is missing", schemaName)
		}
	}

	assertOpenAPIChatContract(t, paths["/chat"].(map[string]any), false)
	assertOpenAPIChatContract(t, paths["/chat/stream"].(map[string]any), true)
	assertOpenAPIHealthContract(t, paths["/health"].(map[string]any))
}

func TestOpenAPIContractMatchesHandlerRoutes(t *testing.T) {
	client := &contractChatClient{
		response: llm.ChatResponse{
			ID:      "chatcmpl-contract",
			Model:   "test-model",
			Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}}},
		},
		stream: &contractStream{},
	}
	handler := NewHandler(client, nil)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{name: "health", method: http.MethodGet, path: "/health", status: http.StatusOK},
		{
			name:   "chat",
			method: http.MethodPost,
			path:   "/chat",
			body:   `{"messages":[{"role":"user","content":"hi"}]}`,
			status: http.StatusOK,
		},
		{
			name:   "chat stream",
			method: http.MethodPost,
			path:   "/chat/stream",
			body:   `{"messages":[{"role":"user","content":"hi"}]}`,
			status: http.StatusOK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.status, response.Body.String())
			}
			assertContractResponse(t, test.path, response)
		})
	}
}

func assertContractResponse(t *testing.T, path string, response *httptest.ResponseRecorder) {
	t.Helper()
	switch path {
	case "/health":
		var body healthResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode health response: %v", err)
		}
		if body.Status != "ok" {
			t.Fatalf("health status = %q, want ok", body.Status)
		}
	case "/chat":
		var body chatResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode chat response: %v", err)
		}
		if body.ID == "" || body.Model == "" || body.Message.Role != llm.RoleAssistant {
			t.Fatalf("unexpected chat response: %+v", body)
		}
	case "/chat/stream":
		events := parseSSEEvents(t, response.Body.String())
		if len(events) != 2 || events[0].Name != "message.delta" || events[1].Name != "message.done" {
			t.Fatalf("unexpected stream events: %+v", events)
		}
		var delta streamDeltaEvent
		if err := json.Unmarshal(events[0].Data, &delta); err != nil {
			t.Fatalf("decode stream delta: %v", err)
		}
		if delta.ID == "" || delta.Model == "" || delta.Sequence != 1 {
			t.Fatalf("unexpected stream delta: %+v", delta)
		}
		var done streamDoneEvent
		if err := json.Unmarshal(events[1].Data, &done); err != nil {
			t.Fatalf("decode stream done: %v", err)
		}
		if done.ID == "" || done.Model == "" {
			t.Fatalf("unexpected stream done: %+v", done)
		}
	}
}

func readOpenAPISpec(t *testing.T) map[string]any {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	data, err := os.ReadFile(filepath.Join(root, "docs", "openapi.json"))
	if err != nil {
		t.Fatalf("read OpenAPI spec: %v", err)
	}

	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("decode OpenAPI spec: %v", err)
	}
	if spec["openapi"] != "3.0.3" {
		t.Fatalf("OpenAPI version = %v, want 3.0.3", spec["openapi"])
	}
	return spec
}

func assertOpenAPIHealthContract(t *testing.T, pathItem map[string]any) {
	t.Helper()
	operation := pathItem["get"].(map[string]any)
	responses := operation["responses"].(map[string]any)
	if _, ok := responses["200"]; !ok {
		t.Fatal("/health GET does not document 200 response")
	}
}

func assertOpenAPIChatContract(t *testing.T, pathItem map[string]any, streaming bool) {
	t.Helper()
	method := "post"
	operation := pathItem[method].(map[string]any)
	requestBody := operation["requestBody"].(map[string]any)
	if requestBody["$ref"] != "#/components/requestBodies/ChatRequest" {
		t.Fatalf("request body ref = %v", requestBody["$ref"])
	}

	responses := operation["responses"].(map[string]any)
	for _, status := range []string{"200", "400", "405", "500", "502", "504"} {
		if _, ok := responses[status]; !ok {
			t.Fatalf("missing %s response for streaming=%t", status, streaming)
		}
	}
	if streaming {
		response := responses["200"].(map[string]any)
		content := response["content"].(map[string]any)
		if _, ok := content["text/event-stream"]; !ok {
			t.Fatal("/chat/stream does not document text/event-stream")
		}
		return
	}

	response := responses["200"].(map[string]any)
	content := response["content"].(map[string]any)
	if _, ok := content["application/json"]; !ok {
		t.Fatal("/chat does not document application/json")
	}
}

type contractChatClient struct {
	response llm.ChatResponse
	stream   llm.ChatStream
}

func (c *contractChatClient) Chat(context.Context, []llm.Message) (llm.ChatResponse, error) {
	return c.response, nil
}

func (c *contractChatClient) ChatStream(context.Context, []llm.Message) (llm.ChatStream, error) {
	return c.stream, nil
}

type contractStream struct {
	done bool
}

func (s *contractStream) Recv() (llm.StreamChunk, error) {
	if s.done {
		return llm.StreamChunk{}, io.EOF
	}
	s.done = true
	return llm.StreamChunk{
		Sequence: 1,
		ID:       "chatcmpl-contract",
		Model:    "test-model",
		Choices: []llm.StreamChoice{{
			Index: 0,
			Delta: llm.Message{Role: llm.RoleAssistant, Content: "ok"},
		}},
	}, nil
}

func (s *contractStream) Close() error {
	return nil
}
