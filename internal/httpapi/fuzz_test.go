package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func FuzzDecodeChatRequest(f *testing.F) {
	f.Add(`{"messages":[{"role":"user","content":"hello"}]}`)
	f.Add(`{"messages":[]}`)
	f.Add(`not-json`)

	handler := NewHandler(nil, nil)
	f.Fuzz(func(t *testing.T, body string) {
		request := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
		_, _ = handler.decodeChatRequest(httptest.NewRecorder(), request)
	})
}

func FuzzValidateChatRequest(f *testing.F) {
	f.Add("user", "hello")
	f.Add("tool", "")
	f.Add("", "content")

	f.Fuzz(func(t *testing.T, role, content string) {
		_ = validateChatRequest(chatRequest{
			Messages: []chatMessage{{Role: role, Content: content}},
		})
	})
}
