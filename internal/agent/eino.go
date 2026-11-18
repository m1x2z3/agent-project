package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/prompt"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
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

// BuildChain 构建 Prompt -> Model -> Lambda 的顺序链。
//
// Prompt 负责把业务变量渲染成模型消息，Model 负责生成，Lambda 负责把
// 框架消息转换成稳定的业务字符串；参数校验和输出整理不交给模型。
func BuildChain(ctx context.Context, chatModel model.BaseChatModel) (compose.Runnable[map[string]any, string], error) {
	if chatModel == nil {
		return nil, ErrNilChatModel
	}

	chain := compose.NewChain[map[string]any, string]()
	chain.AppendChatTemplate(newChatTemplate())
	chain.AppendChatModel(chatModel)
	chain.AppendLambda(compose.InvokableLambda(func(ctx context.Context, message *schema.Message) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if message == nil {
			return "", errors.New("Eino model returned a nil message")
		}
		answer := strings.TrimSpace(message.Content)
		if answer == "" {
			return "", errors.New("Eino model returned an empty message")
		}
		return answer, nil
	}))
	return chain.Compile(ctx)
}

// BuildGraph 构建与 Chain 等价的 Prompt -> Model -> Lambda 图。
func BuildGraph(ctx context.Context, chatModel model.BaseChatModel) (compose.Runnable[map[string]any, string], error) {
	if chatModel == nil {
		return nil, ErrNilChatModel
	}

	graph := compose.NewGraph[map[string]any, string]()
	if err := graph.AddChatTemplateNode("prompt", newChatTemplate()); err != nil {
		return nil, fmt.Errorf("add prompt node: %w", err)
	}
	if err := graph.AddChatModelNode("model", chatModel); err != nil {
		return nil, fmt.Errorf("add model node: %w", err)
	}
	if err := graph.AddLambdaNode("format", compose.InvokableLambda(func(ctx context.Context, message *schema.Message) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if message == nil {
			return "", errors.New("Eino model returned a nil message")
		}
		answer := strings.TrimSpace(message.Content)
		if answer == "" {
			return "", errors.New("Eino model returned an empty message")
		}
		return answer, nil
	})); err != nil {
		return nil, fmt.Errorf("add format node: %w", err)
	}
	if err := graph.AddEdge(compose.START, "prompt"); err != nil {
		return nil, fmt.Errorf("connect graph start: %w", err)
	}
	if err := graph.AddEdge("prompt", "model"); err != nil {
		return nil, fmt.Errorf("connect prompt to model: %w", err)
	}
	if err := graph.AddEdge("model", "format"); err != nil {
		return nil, fmt.Errorf("connect model to format: %w", err)
	}
	if err := graph.AddEdge("format", compose.END); err != nil {
		return nil, fmt.Errorf("connect graph end: %w", err)
	}
	return graph.Compile(ctx)
}

func inputValues(input Input) (map[string]any, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return nil, ErrEmptyQuery
	}
	return map[string]any{"query": query}, nil
}

func newChatTemplate() prompt.ChatTemplate {
	return prompt.FromMessages(
		schema.FString,
		schema.SystemMessage("You are a deterministic development assistant."),
		schema.UserMessage("研发问题：{query}"),
	)
}

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

// Stream implements Eino BaseChatModel's streaming method for completeness.
func (m *DemoChatModel) Stream(ctx context.Context, messages []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}
