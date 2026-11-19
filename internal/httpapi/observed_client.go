package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"agent-project/internal/llm"
)

// observedChatClient 在模型客户端外层记录一次非流式调用的耗时和结果元数据。
//
// 这是 Decorator：业务 Handler 只依赖 HTTPChatClient，不需要显式维护计时器或观测状态。
type observedChatClient struct {
	next   HTTPChatClient
	logger *slog.Logger
}

func (c *observedChatClient) Chat(ctx context.Context, messages []llm.Message) (llm.ChatResponse, error) {
	startedAt := time.Now()
	result, err := c.next.Chat(ctx, messages)
	duration := durationMilliseconds(time.Since(startedAt))

	metadata := requestMetadataFromContext(ctx)
	attrs := []any{
		"request_id", metadata.requestID,
		"path", metadata.path,
		"model_duration_ms", duration,
	}
	if err != nil {
		attrs = append(attrs, "error_type", errorType(err))
	} else {
		attrs = append(attrs,
			"model", result.Model,
			"prompt_tokens", result.Usage.PromptTokens,
			"completion_tokens", result.Usage.CompletionTokens,
			"total_tokens", result.Usage.TotalTokens,
		)
	}
	c.logger.InfoContext(ctx, "model call observed", attrs...)
	return result, err
}

// observedStreamingChatClient 将流式调用包装成 observedChatStream。
type observedStreamingChatClient struct {
	next   HTTPStreamingChatClient
	logger *slog.Logger
}

func (c *observedStreamingChatClient) ChatStream(ctx context.Context, messages []llm.Message) (llm.ChatStream, error) {
	startedAt := time.Now()
	stream, err := c.next.ChatStream(ctx, messages)
	establishedDuration := time.Since(startedAt)
	if err != nil {
		metadata := requestMetadataFromContext(ctx)
		c.logger.InfoContext(ctx, "model stream observed",
			"request_id", metadata.requestID,
			"path", metadata.path,
			"model_duration_ms", durationMilliseconds(establishedDuration),
			"error_type", errorType(err),
		)
		return nil, err
	}
	if stream == nil {
		c.logger.InfoContext(ctx, "model stream observed",
			"model_duration_ms", durationMilliseconds(establishedDuration),
			"error_type", "internal_error",
		)
		return nil, nil
	}

	return &observedChatStream{
		next:                stream,
		logger:              c.logger,
		ctx:                 ctx,
		metadata:            requestMetadataFromContext(ctx),
		startedAt:           startedAt,
		establishedDuration: establishedDuration,
	}, nil
}

// observedChatStream 记录首个有效增量、结束原因、usage 和最终流耗时。
//
// Recv/Close 仍然保持原 ChatStream 契约，观测失败不会改变上游流的返回值。
type observedChatStream struct {
	next                llm.ChatStream
	logger              *slog.Logger
	ctx                 context.Context
	metadata            requestMetadata
	startedAt           time.Time
	establishedDuration time.Duration
	firstTokenAt        time.Time
	firstTokenRecorded  bool
	finishReason        string
	usage               *llm.Usage
	closeOnce           sync.Once
	closeErr            error
	finishOnce          sync.Once
}

func (s *observedChatStream) Recv() (llm.StreamChunk, error) {
	chunk, err := s.next.Recv()
	if err == nil {
		if !s.firstTokenRecorded && len(chunk.Choices) > 0 {
			s.firstTokenAt = time.Now()
			s.firstTokenRecorded = true
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				s.finishReason = *choice.FinishReason
			}
		}
		if chunk.Usage != nil {
			s.usage = chunk.Usage
		}
		return chunk, nil
	}

	if errors.Is(err, io.EOF) {
		s.logFinal("")
	} else if s.ctx.Err() != nil || errors.Is(err, context.Canceled) {
		errorValue := err
		if s.ctx.Err() != nil {
			errorValue = s.ctx.Err()
		}
		s.logFinal(errorType(errorValue))
	} else {
		s.logFinal(errorType(err))
	}
	return chunk, err
}

func (s *observedChatStream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.next.Close()
		errorTypeValue := ""
		if s.ctx.Err() != nil {
			errorTypeValue = errorType(s.ctx.Err())
		}
		s.logFinal(errorTypeValue)
	})
	return s.closeErr
}

func (s *observedChatStream) logFinal(streamErrorType string) {
	s.finishOnce.Do(func() {
		attrs := []any{
			"request_id", s.metadata.requestID,
			"path", s.metadata.path,
			"model_duration_ms", durationMilliseconds(s.establishedDuration),
			"total_duration_ms", durationMilliseconds(time.Since(s.startedAt)),
		}
		if s.firstTokenRecorded {
			attrs = append(attrs, "first_token_duration_ms", durationMilliseconds(s.firstTokenAt.Sub(s.startedAt)))
		}
		if s.finishReason != "" {
			attrs = append(attrs, "finish_reason", s.finishReason)
		}
		if s.usage != nil {
			attrs = append(attrs,
				slog.Group("usage",
					"prompt_tokens", s.usage.PromptTokens,
					"completion_tokens", s.usage.CompletionTokens,
					"total_tokens", s.usage.TotalTokens,
				),
			)
		}
		if streamErrorType != "" {
			attrs = append(attrs, "error_type", streamErrorType)
		}
		s.logger.InfoContext(s.ctx, "model stream observed", attrs...)
	})
}
