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

// StreamingChatClient 是具备流式能力的可选模型客户端接口。
//
// 它独立于 ChatClient，旧的非流式测试替身和调用方无需为了新增接口而修改。
type StreamingChatClient interface {
	ChatStream(context.Context, []llm.Message) (llm.ChatStream, error)
}

// Handler 提供健康检查、非流式聊天和 SSE 流式聊天接口。
type Handler struct {
	client       ChatClient
	streaming    StreamingChatClient
	logger       *slog.Logger
	maxBodyBytes int64
}

type requestMetadata struct {
	requestID string
	method    string
	path      string
	startedAt time.Time
}

type requestMetadataKey struct{}

type responseObserver interface {
	http.ResponseWriter
	Status() int
}

type observedResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

type flushingObservedResponseWriter struct {
	*observedResponseWriter
}

func newObservedResponseWriter(w http.ResponseWriter) responseObserver {
	observed := &observedResponseWriter{ResponseWriter: w}
	if _, ok := w.(http.Flusher); ok {
		return &flushingObservedResponseWriter{observedResponseWriter: observed}
	}
	return observed
}

func (w *observedResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *observedResponseWriter) Status() int {
	return w.status
}

func (w *flushingObservedResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.ResponseWriter.(http.Flusher).Flush()
}

// NewHandler 创建 HTTP API Handler。
func NewHandler(client ChatClient, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	handler := &Handler{
		client:       &observedChatClient{next: client, logger: logger},
		logger:       logger,
		maxBodyBytes: maxRequestBodySize,
	}
	if streamingClient, ok := client.(StreamingChatClient); ok {
		handler.streaming = &observedStreamingChatClient{
			next:   streamingClient,
			logger: logger,
		}
	}
	return handler
}

// ServeHTTP 按路径和方法分发请求，并为接口返回统一 JSON 响应。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	w.Header().Set("X-Request-ID", requestID)
	metadata := requestMetadata{
		requestID: requestID,
		method:    r.Method,
		path:      r.URL.Path,
		startedAt: time.Now(),
	}
	r = r.WithContext(context.WithValue(r.Context(), requestMetadataKey{}, metadata))
	observedWriter := newObservedResponseWriter(w)

	h.serveHTTP(observedWriter, r, requestID)
	h.logRequestTiming(observedWriter, metadata)
}

func (h *Handler) serveHTTP(w responseObserver, r *http.Request, requestID string) {
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
	case "/chat/stream":
		if r.Method != http.MethodPost {
			h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", requestID)
			return
		}
		h.handleChatStream(w, r, requestID)
	default:
		h.writeError(w, http.StatusNotFound, "not_found", "resource not found", requestID)
	}
}

// handleChatStream 将模型流转换为 SSE，并区分流开始前和流开始后的错误。
//
// 在发送 SSE Header 前，错误仍使用普通 JSON 响应；Header 发送后只能发送
// 一个 error 事件。请求 Context 取消时直接停止读取和写入。
func (h *Handler) handleChatStream(w http.ResponseWriter, r *http.Request, requestID string) {
	// 请求一开始就已取消时，不再读取请求体或调用模型。
	if err := r.Context().Err(); err != nil {
		return
	}

	// 复用普通 /chat 的 JSON 解码和请求体大小限制。
	request, err := h.decodeChatRequest(w, r)
	if err != nil {
		// 客户端断开后不能再写错误响应。
		if r.Context().Err() != nil {
			return
		}
		h.writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
		return
	}

	// 复用普通 /chat 的消息数量、角色和内容校验，保证两个接口的输入契约一致。
	if err := validateChatRequest(request); err != nil {
		if r.Context().Err() != nil {
			return
		}
		h.writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
		return
	}

	// 流式接口是可选能力：保留 ChatClient 的同时，通过类型断言获取流式能力，
	// 这样已有只支持非流式 Chat 的测试替身和调用方不需要同步改造。
	if h.streaming == nil {
		h.writeError(w, http.StatusInternalServerError, "internal_error", "streaming model client is unavailable", requestID)
		return
	}

	// SSE 必须支持 Flush，才能把每个增量事件及时发送给客户端。
	// 在写出 SSE Header 前失败，仍可以返回普通 JSON 错误。
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "internal_error", "streaming response is unsupported", requestID)
		return
	}

	// 将 HTTP 层的请求结构转换为模型客户端使用的消息结构。
	messages := make([]llm.Message, len(request.Messages))
	for i, message := range request.Messages {
		messages[i] = llm.Message{
			Role:    message.Role,
			Content: message.Content,
		}
	}

	// 先建立上游模型流，再写出 SSE Header。
	// 这样模型连接失败时，客户端仍能收到原有 JSON 错误响应。
	stream, err := h.streaming.ChatStream(r.Context(), messages)
	if err != nil {
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		h.writeChatError(w, err, requestID)
		return
	}
	if stream == nil {
		h.writeError(w, http.StatusInternalServerError, "internal_error", "streaming model client returned a nil stream", requestID)
		return
	}
	// 无论正常结束、模型报错还是客户端取消，都要关闭上游响应体。
	defer stream.Close()

	// 建流期间请求可能已经被取消，避免此时发送 SSE Header。
	if r.Context().Err() != nil {
		return
	}

	// SSE Header 一旦写出，后续错误只能通过 SSE error 事件表达，
	// 不能再切换成普通 JSON 响应。
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 保存跨 chunk 的最终业务信息：序号用于检测重复/乱序，
	// 其他字段用于组装最后的 message.done 事件。
	var (
		expectedSequence = 1
		responseID       string
		responseModel    string
		usage            *llm.Usage
		finishReason     *string
	)

	for {
		// 客户端断开后立即停止读取上游，避免继续消耗模型资源。
		if err := r.Context().Err(); err != nil {
			return
		}

		// 从模型流读取下一个 SSE 数据块。
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			// io.EOF 表示模型发送了 [DONE]，此时发送最终结束事件。
			// finish_reason 和 usage 都只使用上游实际提供的值。
			doneEvent := streamDoneEvent{
				ID:           responseID,
				Model:        responseModel,
				FinishReason: finishReason,
				Usage:        usage,
			}
			if err := writeSSEEvent(w, flusher, "message.done", doneEvent); err != nil {
				return
			}
			return
		}
		if recvErr != nil {
			// Header 已经写出，模型错误必须转换为 SSE error 事件。
			// 如果是客户端取消，则直接结束，不再写任何响应。
			if r.Context().Err() != nil || errors.Is(recvErr, context.Canceled) {
				return
			}
			h.writeStreamError(w, flusher, streamErrorCode(recvErr), streamErrorMessage(recvErr), requestID)
			return
		}

		// 每个数据块都必须按 1、2、3... 顺序到达，防止重复或乱序事件
		// 破坏客户端拼接结果。
		if chunk.Sequence != expectedSequence {
			h.writeStreamError(w, flusher, "model_error", "stream sequence is out of order", requestID)
			return
		}
		expectedSequence++

		// 记录模型返回的元数据和 usage。部分供应商只在最后的 usage chunk
		// 中返回 Token 统计，因此这里需要跨多个 chunk 保存最新值。
		if chunk.ID != "" {
			responseID = chunk.ID
		}
		if chunk.Model != "" {
			responseModel = chunk.Model
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}

		// 一个 chunk 可能包含多个候选结果，分别转换为 message.delta 事件。
		for _, choice := range chunk.Choices {
			// finish_reason 通常出现在最后一个有 choices 的 chunk 中。
			if choice.FinishReason != nil {
				finishReason = choice.FinishReason
			}
			event := streamDeltaEvent{
				ID:       chunk.ID,
				Model:    chunk.Model,
				Sequence: chunk.Sequence,
				Index:    choice.Index,
				Delta:    choice.Delta,
			}
			// 上游后续 chunk 可能省略 id/model，此时沿用之前保存的元数据。
			if event.ID == "" {
				event.ID = responseID
			}
			if event.Model == "" {
				event.Model = responseModel
			}
			// 每个增量事件写出后立即 Flush，让客户端实时收到内容。
			if err := writeSSEEvent(w, flusher, "message.delta", event); err != nil {
				return
			}
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, eventName string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func (h *Handler) writeStreamError(w http.ResponseWriter, flusher http.Flusher, code, message, requestID string) {
	if err := writeSSEEvent(w, flusher, "error", streamErrorEvent{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	}); err != nil {
		h.logger.Error("write stream error event failed", "request_id", requestID, "error", err)
	}
}

func streamErrorCode(err error) string {
	var apiErr *llm.APIError
	switch {
	case errors.As(err, &apiErr):
		return "model_error"
	case errors.Is(err, context.DeadlineExceeded):
		return "model_timeout"
	default:
		return "internal_error"
	}
}

func streamErrorMessage(err error) string {
	switch streamErrorCode(err) {
	case "model_error":
		return "model service returned an error"
	case "model_timeout":
		return "model service timed out"
	default:
		return "internal server error"
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

func requestMetadataFromContext(ctx context.Context) requestMetadata {
	metadata, _ := ctx.Value(requestMetadataKey{}).(requestMetadata)
	return metadata
}

func (h *Handler) logRequestTiming(w responseObserver, metadata requestMetadata) {
	duration := durationMilliseconds(time.Since(metadata.startedAt))
	attrs := []any{
		"request_id", metadata.requestID,
		"method", metadata.method,
		"path", metadata.path,
		"status", w.Status(),
		"duration_ms", duration,
		"total_duration_ms", duration,
	}
	h.logger.Info("http request completed", attrs...)
}

func durationMilliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func errorType(err error) string {
	var apiErr *llm.APIError
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &apiErr):
		return "model_error"
	default:
		return "internal_error"
	}
}

// decodeChatRequest 将 HTTP 请求体解析为聊天请求。
//
// 这里同时完成协议层防护：限制请求体大小、拒绝空请求或非法 JSON，
// 并确保请求体中只有一个完整 JSON 对象，避免拼接额外内容绕过校验。
func (h *Handler) decodeChatRequest(w http.ResponseWriter, r *http.Request) (decoded chatRequest, err error) {
	parseStartedAt := time.Now()
	defer func() {
		metadata := requestMetadataFromContext(r.Context())
		attrs := []any{
			"request_id", metadata.requestID,
			"path", metadata.path,
			"parse_duration_ms", durationMilliseconds(time.Since(parseStartedAt)),
		}
		if err != nil {
			parseErrorType := "invalid_request"
			if r.Context().Err() != nil {
				parseErrorType = errorType(r.Context().Err())
			}
			attrs = append(attrs, "error_type", parseErrorType)
		}
		h.logger.InfoContext(r.Context(), "http request parsed", attrs...)
	}()

	// MaxBytesReader 会在读取超过限制时返回错误，避免异常请求占用过多内存。
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	// 请求体只使用一次，解析结束后及时关闭底层资源。
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	// 第一次 Decode 读取并解析主 JSON 对象。
	if err := decoder.Decode(&decoded); err != nil {
		return chatRequest{}, fmt.Errorf("invalid JSON body: %w", err)
	}

	var extra json.RawMessage
	// 第二次 Decode 必须读到 EOF；否则说明请求体包含多个 JSON
	// 对象或尾部存在无法接受的额外内容。
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return chatRequest{}, errors.New("request body must contain exactly one JSON object")
		}
		return chatRequest{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	return decoded, nil
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
		case llm.RoleSystem, llm.RoleUser, llm.RoleAssistant:
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

type streamDeltaEvent struct {
	ID       string      `json:"id"`
	Model    string      `json:"model"`
	Sequence int         `json:"sequence"`
	Index    int         `json:"index"`
	Delta    llm.Message `json:"delta"`
}

type streamDoneEvent struct {
	ID           string     `json:"id"`
	Model        string     `json:"model"`
	FinishReason *string    `json:"finish_reason,omitempty"`
	Usage        *llm.Usage `json:"usage,omitempty"`
}

type streamErrorEvent struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

type errorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}
