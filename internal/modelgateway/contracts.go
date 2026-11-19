package modelgateway

import "context"

// Message 是模型网关使用的供应商无关消息结构。
//
// 它刻意不复用 llm.Message，避免业务 Agent 直接依赖 OpenAI 兼容协议包。
type Message struct {
	Role    string
	Content string
}

// ChatRequest 表示一次模型调用请求。
type ChatRequest struct {
	Task     string
	Provider string
	Messages []Message
}

// Usage 表示一次模型调用的 Token 用量。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ChatResponse 是模型网关向业务层暴露的稳定响应结构。
type ChatResponse struct {
	ID           string
	Provider     string
	Model        string
	Message      Message
	FinishReason string
	Usage        *Usage
}

// ProviderHealth 表示供应商的本地配置健康状态。
//
// 本阶段不发起真实探活请求，因此 configured 只代表配置完整且合法，
// 不代表供应商当前网络可用。
type ProviderHealth struct {
	Status ProviderStatus
	Reason string
}

// ProviderStatus 是供应商健康状态枚举。
type ProviderStatus string

const (
	ProviderConfigured ProviderStatus = "configured"
	ProviderHealthy    ProviderStatus = "healthy"
	ProviderUnhealthy  ProviderStatus = "unhealthy"
	ProviderUnknown    ProviderStatus = "unknown"
)

// ModelProvider 是单个模型供应商的统一调用边界。
type ModelProvider interface {
	Name() string
	Chat(context.Context, ChatRequest) (ChatResponse, error)
	Health(context.Context) ProviderHealth
}
