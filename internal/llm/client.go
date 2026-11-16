package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agent-project/internal/config"
)

const maxResponseBodySize = 4 << 20

var (
	// 这些错误供上层通过 errors.Is 判断，避免依赖错误字符串。
	ErrMissingAPIKey = errors.New("llm api key is not configured")
	ErrMissingModel  = errors.New("llm model is not configured")
)

// Message 表示一次模型对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResponse 表示 OpenAI 兼容 Chat Completions API 的响应。
type ChatResponse struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
	Created int64    `json:"created"`
}

// Choice 表示模型返回的一个候选回答。
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage 记录本次模型调用消耗的 Token 数量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Client 封装 OpenAI 兼容模型接口的 HTTP 调用、响应解析和错误转换。
type Client struct {
	baseURL     string
	apiKey      string
	model       string
	temperature float64
	httpClient  *http.Client
	logger      *slog.Logger
}

// NewClient 根据配置创建模型客户端。
//
// logger 为 nil 时使用 slog.Default；模型请求超时时间由 cfg.LLMTimeout 控制。
func NewClient(cfg config.Config, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}

	// 模型请求使用独立 HTTP Client，超时不会影响其他 HTTP 请求。
	httpClient := &http.Client{}
	if cfg.LLMTimeout > 0 {
		httpClient.Timeout = cfg.LLMTimeout
	}

	return &Client{
		baseURL:     strings.TrimRight(cfg.LLMBaseURL, "/"),
		apiKey:      strings.TrimSpace(cfg.LLMAPIKey),
		model:       strings.TrimSpace(cfg.LLMModel),
		temperature: cfg.LLMTemperature,
		httpClient:  httpClient,
		logger:      logger,
	}
}

// Chat 向模型发送非流式对话请求，并返回模型生成的第一个完整响应。
//
// ctx 控制请求生命周期，messages 不能为空。网络错误、上下文取消、上游非 2xx
// 响应和无效响应体都会通过 error 返回。
func (c *Client) Chat(ctx context.Context, messages []Message) (ChatResponse, error) {
	// 先做本地校验，避免无效请求消耗网络和模型额度。
	if err := ctx.Err(); err != nil {
		return ChatResponse{}, fmt.Errorf("llm chat canceled before request: %w", err)
	}
	if c.apiKey == "" {
		return ChatResponse{}, ErrMissingAPIKey
	}
	if c.model == "" {
		return ChatResponse{}, ErrMissingModel
	}
	if len(messages) == 0 {
		return ChatResponse{}, errors.New("llm chat requires at least one message")
	}

	requestBody := chatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: c.temperature,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal llm request: %w", err)
	}

	endpoint := c.baseURL + "/chat/completions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("create llm request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")

	startedAt := time.Now()
	// 日志只记录请求元数据，不记录 API Key 和完整消息内容。
	c.logger.InfoContext(ctx, "llm request",
		"method", http.MethodPost,
		"endpoint", endpoint,
		"model", c.model,
		"message_count", len(messages),
	)

	response, err := c.httpClient.Do(request)
	if err != nil {
		c.logger.ErrorContext(ctx, "llm request failed",
			"model", c.model,
			"duration", time.Since(startedAt).String(),
			"error", err,
		)
		return ChatResponse{}, fmt.Errorf("send llm request: %w", err)
	}
	defer response.Body.Close()

	// 限制响应体大小，避免异常上游返回超大内容占用内存。
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodySize))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read llm response: %w", err)
	}

	requestID := response.Header.Get("X-Request-ID")
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// 将供应商错误统一转换为 APIError，便于 HTTP 层返回一致结构。
		apiErr := parseAPIError(response.StatusCode, requestID, responseBody)
		c.logger.ErrorContext(ctx, "llm response error",
			"status", response.StatusCode,
			"request_id", requestID,
			"duration", time.Since(startedAt).String(),
			"error", apiErr,
		)
		return ChatResponse{}, apiErr
	}

	var result ChatResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return ChatResponse{}, fmt.Errorf("decode llm response: %w", err)
	}
	if len(result.Choices) == 0 {
		return ChatResponse{}, errors.New("llm response contains no choices")
	}

	c.logger.InfoContext(ctx, "llm response",
		"status", response.StatusCode,
		"request_id", requestID,
		"duration", time.Since(startedAt).String(),
		"model", result.Model,
		"choice_count", len(result.Choices),
		"total_tokens", result.Usage.TotalTokens,
	)
	return result, nil
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
}

type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    any    `json:"code"`
}

// APIError 表示模型服务返回的非 2xx 错误响应。
//
// 它保留 HTTP 状态码、上游请求 ID 和供应商错误字段，供上层统一转换为 HTTP 错误响应。
type APIError struct {
	StatusCode int
	RequestID  string
	Message    string
	Type       string
	Param      string
	Code       any
}

// Error 返回包含状态码、请求 ID 和错误消息的可读错误文本。
func (e *APIError) Error() string {
	message := e.Message
	if message == "" {
		message = http.StatusText(e.StatusCode)
	}
	if e.RequestID == "" {
		return fmt.Sprintf("llm api error: status=%d message=%s", e.StatusCode, message)
	}
	return fmt.Sprintf("llm api error: status=%d request_id=%s message=%s", e.StatusCode, e.RequestID, message)
}

// parseAPIError 将供应商错误 JSON 转换为项目统一的 APIError。
func parseAPIError(statusCode int, requestID string, body []byte) *APIError {
	// 即使上游返回非标准错误 JSON，也保留 HTTP 状态码和可用的请求 ID。
	apiErr := &APIError{
		StatusCode: statusCode,
		RequestID:  requestID,
		Message:    http.StatusText(statusCode),
	}

	var envelope apiErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil {
		apiErr.Message = envelope.Error.Message
		apiErr.Type = envelope.Error.Type
		apiErr.Param = envelope.Error.Param
		apiErr.Code = envelope.Error.Code
	}
	return apiErr
}
