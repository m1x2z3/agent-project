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
	ErrMissingAPIKey = errors.New("llm api key is not configured")
	ErrMissingModel  = errors.New("llm model is not configured")
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
	Created int64    `json:"created"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Client struct {
	baseURL     string
	apiKey      string
	model       string
	temperature float64
	httpClient  *http.Client
	logger      *slog.Logger
}

func NewClient(cfg config.Config, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}

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

func (c *Client) Chat(ctx context.Context, messages []Message) (ChatResponse, error) {
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

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodySize))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read llm response: %w", err)
	}

	requestID := response.Header.Get("X-Request-ID")
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
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

type APIError struct {
	StatusCode int
	RequestID  string
	Message    string
	Type       string
	Param      string
	Code       any
}

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

func parseAPIError(statusCode int, requestID string, body []byte) *APIError {
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
