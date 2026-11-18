package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agent-project/internal/config"
)

const (
	maxResponseBodySize = 4 << 20
	maxStreamEventSize  = 1 << 20
)

var (
	// 这些错误供上层通过 errors.Is 判断，避免依赖错误字符串。
	ErrMissingAPIKey    = errors.New("llm api key is not configured")
	ErrMissingModel     = errors.New("llm model is not configured")
	ErrInvalidStream    = errors.New("invalid llm stream")
	ErrResponseTooLarge = errors.New("llm response body exceeds size limit")
)

const (
	// RoleSystem 表示系统级指令或上下文。
	RoleSystem = "system"
	// RoleUser 表示用户输入。
	RoleUser = "user"
	// RoleAssistant 表示模型生成的消息。
	RoleAssistant = "assistant"
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

// ChatStream 表示一个可以逐块读取模型输出的流。
//
// Recv 返回 io.EOF 表示上游发送了 [DONE]；调用方仍应在结束后调用 Close。
type ChatStream interface {
	Recv() (StreamChunk, error)
	Close() error
}

// StreamChunk 表示 OpenAI 兼容流式响应中的一个 SSE 数据块。
type StreamChunk struct {
	// Sequence 在上游未提供序号时由客户端生成，仅用于对外事件编号。
	// SequenceFromUpstream 为 true 时，Sequence 才代表上游明确提供的序号。
	Sequence             int
	SequenceFromUpstream bool
	ID                   string
	Model                string
	Choices              []StreamChoice
	Usage                *Usage
}

// StreamChoice 表示流式响应中的一个增量候选。
type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        Message `json:"delta"`
	FinishReason *string `json:"finish_reason"`
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
		"endpoint", sanitizeEndpoint(request.URL),
		"model", c.model,
		"message_count", len(messages),
	)

	response, err := c.httpClient.Do(request)
	if err != nil {
		c.logger.ErrorContext(ctx, "llm request failed",
			"model", c.model,
			"duration_ms", durationMilliseconds(time.Since(startedAt)),
			"error_type", requestErrorType(err),
		)
		return ChatResponse{}, fmt.Errorf("send llm request: %w", err)
	}
	defer response.Body.Close()

	// 限制响应体大小，避免异常上游返回超大内容占用内存。
	responseBody, err := readResponseBody(response.Body, maxResponseBodySize)
	if err != nil {
		if (response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices) &&
			errors.Is(err, ErrResponseTooLarge) {
			return ChatResponse{}, &APIError{
				StatusCode: response.StatusCode,
				RequestID:  response.Header.Get("X-Request-ID"),
				Message:    "upstream error response unavailable",
			}
		}
		return ChatResponse{}, fmt.Errorf("read llm response: %w", err)
	}

	requestID := response.Header.Get("X-Request-ID")
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// 将供应商错误统一转换为 APIError，便于 HTTP 层返回一致结构。
		apiErr := parseAPIError(response.StatusCode, requestID, responseBody)
		c.logger.ErrorContext(ctx, "llm response error",
			"status", response.StatusCode,
			"request_id", requestID,
			"duration_ms", durationMilliseconds(time.Since(startedAt)),
			"error_type", "api_error",
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
		"duration_ms", durationMilliseconds(time.Since(startedAt)),
		"model", result.Model,
		"choice_count", len(result.Choices),
		"total_tokens", result.Usage.TotalTokens,
	)
	return result, nil
}

// ChatStream 向模型发起流式对话请求，并返回一个按 SSE 数据块读取的流。
//
// 请求体和鉴权方式与 Chat 保持一致，只额外设置 stream=true。响应体的所有权
// 转移给 chatStream，由调用方通过 Close 释放。
func (c *Client) ChatStream(ctx context.Context, messages []Message) (ChatStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("llm stream canceled before request: %w", err)
	}
	if c.apiKey == "" {
		return nil, ErrMissingAPIKey
	}
	if c.model == "" {
		return nil, ErrMissingModel
	}
	if len(messages) == 0 {
		return nil, errors.New("llm stream requires at least one message")
	}

	requestBody := chatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: c.temperature,
		Stream:      true,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("marshal llm stream request: %w", err)
	}

	endpoint := c.baseURL + "/chat/completions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create llm stream request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")

	startedAt := time.Now()
	c.logger.InfoContext(ctx, "llm stream request",
		"method", http.MethodPost,
		"endpoint", sanitizeEndpoint(request.URL),
		"model", c.model,
		"message_count", len(messages),
	)

	response, err := c.httpClient.Do(request)
	if err != nil {
		c.logger.ErrorContext(ctx, "llm stream request failed",
			"model", c.model,
			"duration_ms", durationMilliseconds(time.Since(startedAt)),
			"error_type", requestErrorType(err),
		)
		return nil, fmt.Errorf("send llm stream request: %w", err)
	}

	requestID := response.Header.Get("X-Request-ID")
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		responseBody, readErr := readResponseBody(response.Body, maxResponseBodySize)
		if readErr != nil {
			if errors.Is(readErr, ErrResponseTooLarge) {
				return nil, &APIError{
					StatusCode: response.StatusCode,
					RequestID:  requestID,
					Message:    "upstream error response unavailable",
				}
			}
			return nil, fmt.Errorf("read llm stream error response: %w", readErr)
		}
		apiErr := parseAPIError(response.StatusCode, requestID, responseBody)
		c.logger.ErrorContext(ctx, "llm stream response error",
			"status", response.StatusCode,
			"request_id", requestID,
			"duration_ms", durationMilliseconds(time.Since(startedAt)),
			"error_type", "api_error",
		)
		return nil, apiErr
	}

	return &chatStream{
		reader:    bufio.NewReader(response.Body),
		body:      response.Body,
		logger:    c.logger,
		model:     c.model,
		requestID: requestID,
	}, nil
}

func durationMilliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

// sanitizeEndpoint 用白名单字段构建日志端点，避免 userinfo、query 和 fragment 泄露凭据。
func sanitizeEndpoint(endpoint *url.URL) string {
	return (&url.URL{
		Scheme:  endpoint.Scheme,
		Host:    endpoint.Host,
		Path:    endpoint.Path,
		RawPath: endpoint.RawPath,
	}).String()
}

// requestErrorType 保留排障需要的错误分类，不记录可能携带敏感 URL 的错误原文。
func requestErrorType(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "network_error"
	}
}

func readResponseBody(reader io.Reader, maxSize int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxSize {
		return nil, fmt.Errorf("%w: limit=%d", ErrResponseTooLarge, maxSize)
	}
	return body, nil
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream,omitempty"`
}

type chatStream struct {
	reader        *bufio.Reader
	body          io.ReadCloser
	logger        *slog.Logger
	model         string
	requestID     string
	sequence      int
	done          bool
	eofAfterEvent bool
	closeOnce     sync.Once
	closeErr      error
}

// Recv 读取下一个 SSE 数据块；只有收到 [DONE] 后才返回 io.EOF。
//
// 这里的循环不是把一个文本块拆成多次读取，而是为了跳过空事件、
// 注释事件等没有业务数据的 SSE 事件，直到拿到一个有效 chunk。
// 具体的逐行读取和多行 data 拼接由 readEvent 完成。
func (s *chatStream) Recv() (StreamChunk, error) {
	if s.done {
		return StreamChunk{}, io.EOF
	}
	if s.eofAfterEvent {
		return StreamChunk{}, fmt.Errorf("%w: data event was not followed by [DONE]", ErrInvalidStream)
	}

	// 外层循环：不断获取 SSE 事件，跳过空事件，直到返回一个有效 chunk。
	for {
		data, err := s.readEvent()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return StreamChunk{}, fmt.Errorf("%w: stream ended before [DONE]", ErrInvalidStream)
			}
			return StreamChunk{}, fmt.Errorf("%w: %v", ErrInvalidStream, err)
		}
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			s.done = true
			return StreamChunk{}, io.EOF
		}
		var wire struct {
			Sequence *int           `json:"sequence"`
			ID       string         `json:"id"`
			Model    string         `json:"model"`
			Choices  []StreamChoice `json:"choices"`
			Usage    *Usage         `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &wire); err != nil {
			return StreamChunk{}, fmt.Errorf("%w: decode data event: %v", ErrInvalidStream, err)
		}
		if len(wire.Choices) == 0 && wire.Usage == nil {
			return StreamChunk{}, fmt.Errorf("%w: data event contains no choices or usage", ErrInvalidStream)
		}

		s.sequence++
		sequence := s.sequence
		sequenceFromUpstream := false
		if wire.Sequence != nil {
			sequence = *wire.Sequence
			sequenceFromUpstream = true
		}
		return StreamChunk{
			Sequence:             sequence,
			SequenceFromUpstream: sequenceFromUpstream,
			ID:                   wire.ID,
			Model:                wire.Model,
			Choices:              wire.Choices,
			Usage:                wire.Usage,
		}, nil
	}
}

// Close 关闭底层响应体；重复调用只会关闭一次。
func (s *chatStream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.body.Close()
	})
	return s.closeErr
}

// readEvent 按 SSE 协议逐行读取并拼接一个完整事件。
//
// SSE 使用空行表示事件结束，因此一个事件可能包含多行 data:
// 字段。这里的循环会持续读取这些行，直到遇到空行，再把多行 data
// 用换行符拼接后返回；它不是在重复读取同一个业务 chunk。
func (s *chatStream) readEvent() (string, error) {
	var data strings.Builder
	hasData := false
	eventSize := 0

	// 逐行读取当前 SSE 事件，直到遇到空行表示事件结束。
	for {
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if line != "" {
					if err := processEventLine(line, &data, &hasData, &eventSize); err != nil {
						return "", err
					}
				}
				// 允许最后一个事件没有额外的空行；调用方会继续读取并验证
				// 它是否确实是 [DONE]，避免把正常结束误判为半包。
				if hasData {
					s.eofAfterEvent = true
					return data.String(), nil
				}
				return "", io.ErrUnexpectedEOF
			}
			return "", err
		}

		if line == "" {
			if hasData {
				// 空行表示当前 SSE 事件结束，交给 Recv 解析 data 内容。
				return data.String(), nil
			}
			continue
		}
		if err := processEventLine(line, &data, &hasData, &eventSize); err != nil {
			return "", err
		}
	}
}

func (s *chatStream) readLine() (string, error) {
	line := make([]byte, 0, 128)
	for {
		part, isPrefix, err := s.reader.ReadLine()
		if len(part) > maxStreamEventSize-len(line) {
			return "", fmt.Errorf("%w: event exceeds %d bytes", ErrInvalidStream, maxStreamEventSize)
		}
		line = append(line, part...)
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return string(line), io.EOF
			}
			return "", err
		}
		if !isPrefix {
			if len(line) > maxStreamEventSize {
				return "", fmt.Errorf("%w: event exceeds %d bytes", ErrInvalidStream, maxStreamEventSize)
			}
			return string(line), err
		}
	}
}

func processEventLine(line string, data *strings.Builder, hasData *bool, eventSize *int) error {
	line = strings.TrimSuffix(line, "\r")
	*eventSize += len(line)
	if *eventSize > maxStreamEventSize {
		return fmt.Errorf("%w: event exceeds %d bytes", ErrInvalidStream, maxStreamEventSize)
	}

	if strings.HasPrefix(line, ":") {
		return nil
	}
	switch {
	case strings.HasPrefix(line, "data:"):
		value := strings.TrimPrefix(line, "data:")
		value = strings.TrimPrefix(value, " ")
		if *hasData {
			data.WriteByte('\n')
		}
		data.WriteString(value)
		*hasData = true
	case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
		// These SSE fields are valid but are not needed by the model protocol.
	default:
		return fmt.Errorf("%w: unsupported SSE field", ErrInvalidStream)
	}
	return nil
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
