package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
)

var (
	// ErrEmptyQuery 表示调用方没有提供可供 Agent 处理的问题。
	ErrEmptyQuery = errors.New("agent query must not be empty")
	// ErrNilChatModel 表示编排链缺少模型节点。
	ErrNilChatModel = errors.New("agent chat model must not be nil")
)

// Input 是 Eino 编排层的业务输入。
//
// HTTP 请求会在 API 层先转换为业务输入；Eino 的 schema.Message 不向 HTTP
// 层泄漏，避免编排框架类型成为外部接口契约。
type Input struct {
	Query string
}

// Output 是 Eino 编排层的业务输出。
type Output struct {
	Answer string
}

// Pipeline 同时持有 Chain 和 Graph 两种等价的最小执行方式。
//
// 后续 Agent Runtime 可以只依赖 Run/RunGraph，不需要知道模型供应商的 HTTP
// 协议，也不需要把 Eino 组件直接暴露给 HTTP Handler。
type Pipeline struct {
	chain compose.Runnable[map[string]any, string]
	graph compose.Runnable[map[string]any, string]
}

// NewPipeline 构建最小 Eino Pipeline。
//
// chatModel 由调用方注入，当前可以使用 DemoChatModel 或测试替身；真实供应商
// 适配仍由后续模型网关负责，本阶段不把 internal/llm 改造成 Eino 类型。
func NewPipeline(ctx context.Context, chatModel model.BaseChatModel) (*Pipeline, error) {
	if chatModel == nil {
		return nil, ErrNilChatModel
	}

	chain, err := BuildChain(ctx, chatModel)
	if err != nil {
		return nil, fmt.Errorf("build Eino chain: %w", err)
	}
	graph, err := BuildGraph(ctx, chatModel)
	if err != nil {
		return nil, fmt.Errorf("build Eino graph: %w", err)
	}
	return &Pipeline{chain: chain, graph: graph}, nil
}

// Run 使用 Chain 执行一次请求。
func (p *Pipeline) Run(ctx context.Context, input Input) (Output, error) {
	return p.RunChain(ctx, input)
}

// RunChain 使用 Eino Chain 执行请求。
func (p *Pipeline) RunChain(ctx context.Context, input Input) (Output, error) {
	values, err := inputValues(input)
	if err != nil {
		return Output{}, err
	}
	answer, err := p.chain.Invoke(ctx, values)
	if err != nil {
		return Output{}, fmt.Errorf("run Eino chain: %w", err)
	}
	return Output{Answer: answer}, nil
}

// RunGraph 使用 Eino Graph 执行同一条业务链路，用于验证图式编排边界。
func (p *Pipeline) RunGraph(ctx context.Context, input Input) (Output, error) {
	values, err := inputValues(input)
	if err != nil {
		return Output{}, err
	}
	answer, err := p.graph.Invoke(ctx, values)
	if err != nil {
		return Output{}, fmt.Errorf("run Eino graph: %w", err)
	}
	return Output{Answer: answer}, nil
}
