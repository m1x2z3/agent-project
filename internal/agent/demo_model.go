package agent

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// DemoChatModel 是当天使用的确定性模型替身。
//
// 它只用于验证 Eino 的 Model 节点、Context 和错误传播，不访问真实模型服务。
// 后续模型网关会将 internal/llm 的供应商适配器转换为 Eino BaseChatModel。
type DemoChatModel struct {
	Prefix string
	Err    error
}

// NewDemoChatModel 创建一个不会产生网络请求的确定性模型。
func NewDemoChatModel() *DemoChatModel {
	return &DemoChatModel{Prefix: "demo answer: "}
}

// Generate 实现 Eino BaseChatModel 的非流式调用。
func (m *DemoChatModel) Generate(ctx context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.Err != nil {
		return nil, m.Err
	}
	if len(messages) == 0 {
		return nil, errors.New("demo model requires at least one message")
	}
	last := messages[len(messages)-1]
	if last == nil {
		return nil, errors.New("demo model received a nil message")
	}
	return schema.AssistantMessage(m.Prefix+last.Content, nil), nil
}

// Stream 实现 Eino BaseChatModel 的流式调用。
func (m *DemoChatModel) Stream(ctx context.Context, messages []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}
