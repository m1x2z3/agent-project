package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agent-project/internal/llm"
)

const (
	maxRequestBodySize = 1 << 20
	maxMessageCount    = 32
)

var (
	errMessagesEmpty   = errors.New("messages must not be empty")
	errTooManyMessages = errors.New("messages must not contain more than 32 items")
)

// ChatClient 是 HTTP 层依赖的最小模型客户端接口，便于隔离真实模型服务进行测试。
type ChatClient interface {
	Chat(context.Context, []llm.Message) (llm.ChatResponse, error)
}

// Handler 提供健康检查和非流式聊天接口。
type Handler struct {
	client       ChatClient
	logger       *slog.Logger
	maxBodyBytes int64
}

// NewHandler 创建 HTTP API Handler。
func NewHandler(client ChatClient, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		client:       client,
		logger:       logger,
		maxBodyBytes: maxRequestBodySize,
	}
}

// ServeHTTP 按路径和方法分发请求，并为接口返回统一 JSON 响应。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	w.Header().Set("X-Request-ID", requestID)

	switch r.URL.Path {
	case "/health":
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", requestID)
			return
		}
		h.handleHealth(w)
	case "/chat":
		if r.Method != http.MethodPost {
			h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", requestID)
			return
		}
		h.handleChat(w, r, requestID)
	default:
		h.writeError(w, http.StatusNotFound, "not_found", "resource not found", requestID)
	}
}

// handleHealth 返回服务进程可用状态，不调用模型服务。
func (h *Handler) handleHealth(w http.ResponseWriter) {
	h.writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// handleChat 校验请求、调用模型客户端，并将供应商响应转换为业务响应。
func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request, requestID string) {
	if err := r.Context().Err(); err != nil {
		return
	}

	request, err := h.decodeChatRequest(w, r)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		h.writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
		return
	}
	if err := validateChatRequest(request); err != nil {
		if r.Context().Err() != nil {
			return
		}
		h.writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
		return
	}

	messages := make([]llm.Message, len(request.Messages))
	for i, message := range request.Messages {
		messages[i] = llm.Message{
			Role:    message.Role,
			Content: message.Content,
		}
	}

	result, err := h.client.Chat(r.Context(), messages)
	if err != nil {
		// 客户端断开后不再尝试写响应，避免无意义的错误和日志。
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		h.writeChatError(w, err, requestID)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if len(result.Choices) == 0 {
		h.writeError(w, http.StatusInternalServerError, "internal_error", "model response contains no choices", requestID)
		return
	}

	response := chatResponse{
		ID:      result.ID,
		Model:   result.Model,
		Message: result.Choices[0].Message,
		Usage:   result.Usage,
	}
	h.writeJSON(w, http.StatusOK, response)
}

// decodeChatRequest 限制请求体大小，并确保请求体只包含一个完整 JSON 对象。
func (h *Handler) decodeChatRequest(w http.ResponseWriter, r *http.Request) (chatRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	defer r.Body.Close()

	var request chatRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil {
		return chatRequest{}, fmt.Errorf("invalid JSON body: %w", err)
	}

	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return chatRequest{}, errors.New("request body must contain exactly one JSON object")
		}
		return chatRequest{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	return request, nil
}

// validateChatRequest 检查消息数量、角色和消息内容，拒绝不满足接口契约的请求。
func validateChatRequest(request chatRequest) error {
	if len(request.Messages) == 0 {
		return errMessagesEmpty
	}
	if len(request.Messages) > maxMessageCount {
		return errTooManyMessages
	}

	for index, message := range request.Messages {
		switch message.Role {
		case "system", "user", "assistant":
		default:
			return fmt.Errorf("messages[%d].role must be system, user, or assistant", index)
		}
		if strings.TrimSpace(message.Content) == "" {
			return fmt.Errorf("messages[%d].content must not be empty", index)
		}
	}
	return nil
}

// writeChatError 将模型调用错误映射为客户端可理解的统一 HTTP 错误。
func (h *Handler) writeChatError(w http.ResponseWriter, err error, requestID string) {
	var apiErr *llm.APIError
	switch {
	case errors.As(err, &apiErr):
		h.writeError(w, http.StatusBadGateway, "model_error", "model service returned an error", requestID)
	case errors.Is(err, context.DeadlineExceeded):
		h.writeError(w, http.StatusGatewayTimeout, "model_timeout", "model service timed out", requestID)
	default:
		h.logger.Error("chat request failed", "request_id", requestID, "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", requestID)
	}
}

// writeJSON 写入统一的 JSON 响应头、状态码和响应体。
func (h *Handler) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		h.logger.Error("write JSON response failed", "status", status, "error", err)
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	h.writeJSON(w, status, errorResponse{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	})
}

// newRequestID 为每个请求生成可用于日志关联和故障排查的请求 ID。
func newRequestID() string {
	var randomID [8]byte
	if _, err := rand.Read(randomID[:]); err == nil {
		return "req-" + hex.EncodeToString(randomID[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

type chatRequest struct {
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type healthResponse struct {
	Status string `json:"status"`
}

type chatResponse struct {
	ID      string      `json:"id"`
	Model   string      `json:"model"`
	Message llm.Message `json:"message"`
	Usage   llm.Usage   `json:"usage"`
}

type errorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}
