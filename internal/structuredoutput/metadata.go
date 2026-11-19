package structuredoutput

import "context"

// ExecutionMetadata 记录本次解析使用的供应商、模型和各类版本信息。
type ExecutionMetadata struct {
	Provider           string
	Model              string
	PromptVersion      string
	SchemaVersion      string
	ModelConfigVersion string
}

// RepairRequest 包含解析失败的模型输出，以及可以安全传递给修复逻辑的元数据。
type RepairRequest struct {
	Kind     OutputKind
	Raw      []byte
	Reason   string
	Metadata ExecutionMetadata
}

// RepairFunc 为非法模型输出提供一次可选的修复机会。
type RepairFunc func(context.Context, RepairRequest) ([]byte, error)

// DecodeOptions 控制结构化输出的解析过程。
type DecodeOptions struct {
	Metadata ExecutionMetadata
	Repair   RepairFunc
}

// DecodeReport 描述解析结果，但不保存模型原始内容。
type DecodeReport struct {
	Kind     OutputKind
	Metadata ExecutionMetadata
	Repaired bool
	Attempts int
}
