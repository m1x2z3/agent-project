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
	chain.AppendLambda(compose.InvokableLambda(formatModelMessage))
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
	if err := graph.AddLambdaNode("format", compose.InvokableLambda(formatModelMessage)); err != nil {
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

func formatModelMessage(ctx context.Context, message *schema.Message) (string, error) {
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
