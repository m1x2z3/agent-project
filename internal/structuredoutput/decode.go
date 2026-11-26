package structuredoutput

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// DecodeTaskAnalysis 是解析任务分析结果的类型安全入口。
// 它把 TaskAnalysis 类型和对应的业务语义校验绑定到统一解析流程中。
func DecodeTaskAnalysis(ctx context.Context, raw []byte, options DecodeOptions) (TaskAnalysis, DecodeReport, error) {
	return decode(ctx, OutputTaskAnalysis, raw, options, unmarshalTaskAnalysis, TaskAnalysis.Validate)
}

// DecodeToolDecision 是解析工具决策结果的类型安全入口。
func DecodeToolDecision(ctx context.Context, raw []byte, options DecodeOptions) (ToolDecision, DecodeReport, error) {
	return decode(ctx, OutputToolDecision, raw, options, unmarshalToolDecision, ToolDecision.Validate)
}

// DecodeFinalAnswer 是解析最终回答结果的类型安全入口。
func DecodeFinalAnswer(ctx context.Context, raw []byte, options DecodeOptions) (FinalAnswer, DecodeReport, error) {
	return decode(ctx, OutputFinalAnswer, raw, options, unmarshalFinalAnswer, FinalAnswer.Validate)
}

// unmarshalFunc 负责把通过 Schema 校验的 JSON 转换为具体业务结果。
type unmarshalFunc[T any] func([]byte) (T, error)

// semanticValidateFunc 负责执行无法仅通过 JSON 结构表达的业务规则。
type semanticValidateFunc[T any] func(T) error

// decode 执行统一的元数据校验、Schema 校验、业务语义校验和一次修复流程。
// 通过泛型回调复用公共流程，同时让对外的三个入口保持类型安全。
func decode[T any](
	ctx context.Context,
	kind OutputKind,
	raw []byte,
	options DecodeOptions,
	unmarshalValue unmarshalFunc[T],
	validateValue semanticValidateFunc[T],
) (T, DecodeReport, error) {
	var zero T
	spec, err := schemaFor(kind)
	if err != nil {
		return zero, DecodeReport{}, err
	}
	if err := validateMetadata(options.Metadata, spec); err != nil {
		return zero, DecodeReport{}, &DecodeError{Kind: kind, Stage: stageMetadata, Cause: err}
	}
	if err := ctx.Err(); err != nil {
		return zero, DecodeReport{}, err
	}

	validator, err := compileSchema(spec)
	if err != nil {
		return zero, DecodeReport{}, fmt.Errorf("compile %s schema: %w", kind, err)
	}

	// 第一次尝试必须同时通过结构校验和业务语义校验。
	value, validationErr := validateAndDecode(raw, validator, unmarshalValue, validateValue)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return zero, DecodeReport{}, ctxErr
	}
	if validationErr == nil {
		return value, DecodeReport{
			Kind:     kind,
			Metadata: options.Metadata,
			Attempts: 1,
		}, nil
	} else {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, DecodeReport{}, ctxErr
		}
		if options.Repair == nil {
			return zero, DecodeReport{}, &DecodeError{
				Kind:  kind,
				Stage: validationStageForError(validationErr),
				Cause: fmt.Errorf("%w: %w", ErrRepairUnavailable, validationErr),
			}
		}

		// 修复逻辑通过依赖注入提供，使本包不依赖任何具体模型 SDK。
		// 对同一份非法输出最多只给调用方一次修复机会，避免无限重试。
		repaired, repairErr := options.Repair(ctx, RepairRequest{
			Kind:     kind,
			Raw:      append([]byte(nil), raw...),
			Reason:   validationReason(validationErr),
			Metadata: options.Metadata,
		})
		if repairErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return zero, DecodeReport{}, ctxErr
			}
			return zero, DecodeReport{}, &DecodeError{
				Kind:  kind,
				Stage: stageRepair,
				// 保留稳定的 ErrRepairFailed 分类，同时让调用方通过错误链识别
				// 预算超限等结构化原因；DecodeError.Error 仍不会暴露底层错误文本。
				Cause: errors.Join(ErrRepairFailed, repairErr),
			}
		}
		if err := ctx.Err(); err != nil {
			return zero, DecodeReport{}, err
		}
		// 修复结果不能直接信任，必须重新经过完整的 JSON、Schema 和业务校验流程。
		value, repairedErr := validateAndDecode(repaired, validator, unmarshalValue, validateValue)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, DecodeReport{}, ctxErr
		}
		if repairedErr == nil {
			return value, DecodeReport{
				Kind:     kind,
				Metadata: options.Metadata,
				Repaired: true,
				Attempts: 2,
			}, nil
		}
		return zero, DecodeReport{}, &DecodeError{
			Kind:  kind,
			Stage: stageRepair,
			Cause: ErrRepairRejected,
		}
	}
}

// compileSchema 把生成的 JSON Schema 文档编译成可执行的校验器。
func compileSchema(spec schemaSpec) (*jsonschema.Schema, error) {
	data, err := marshalSchema(spec)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	const schemaURL = "inmemory://structured-output/schema.json"
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return nil, err
	}
	return compiler.Compile(schemaURL)
}

// validateAndDecode 明确拆分两层校验职责：
// 先由 Schema 校验 JSON 结构，再由 Go 代码校验业务语义。
func validateAndDecode[T any](
	raw []byte,
	validator *jsonschema.Schema,
	unmarshalValue unmarshalFunc[T],
	validateValue semanticValidateFunc[T],
) (T, error) {
	var zero T
	normalized, err := strictJSON(raw)
	if err != nil {
		return zero, err
	}
	// Schema 校验器使用解析后的 JSON 实例；
	// 标准化后的字节则继续用于转换成目标 Go 类型。
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(normalized))
	if err != nil {
		return zero, ErrInvalidJSON
	}
	// Schema 层负责 required 字段、类型、枚举、数值范围和额外字段等结构规则。
	// 这些规则适用于该结果类型的所有调用方。
	if err := validator.Validate(instance); err != nil {
		return zero, fmt.Errorf("%w: schema rejected output", ErrSchemaValidation)
	}
	// 只有通过 Schema 校验的 JSON，才允许转换成具体业务对象。
	value, err := unmarshalValue(normalized)
	if err != nil {
		return zero, fmt.Errorf("%w: unable to decode validated JSON", ErrInvalidJSON)
	}
	// 业务语义层负责跨字段约束，以及依赖状态的业务规则。
	if err := validateValue(value); err != nil {
		return zero, fmt.Errorf("%w: semantic validation failed", ErrSemanticValidation)
	}
	return value, nil
}

// unmarshalTaskAnalysis 只负责把 JSON 转换为 TaskAnalysis。
// 它不执行业务校验，业务校验由后续的 Validate 方法负责。
func unmarshalTaskAnalysis(data []byte) (TaskAnalysis, error) {
	var value TaskAnalysis
	if err := json.Unmarshal(data, &value); err != nil {
		return TaskAnalysis{}, err
	}
	return value, nil
}

// unmarshalToolDecision 只负责把 JSON 转换为 ToolDecision。
func unmarshalToolDecision(data []byte) (ToolDecision, error) {
	var value ToolDecision
	if err := json.Unmarshal(data, &value); err != nil {
		return ToolDecision{}, err
	}
	return value, nil
}

// unmarshalFinalAnswer 只负责把 JSON 转换为 FinalAnswer。
func unmarshalFinalAnswer(data []byte) (FinalAnswer, error) {
	var value FinalAnswer
	if err := json.Unmarshal(data, &value); err != nil {
		return FinalAnswer{}, err
	}
	return value, nil
}

// strictJSON 只接受一个完整 JSON 值，并拒绝尾部出现的第二个 JSON 值。
// 这样可以避免把格式错误的模型输出误判为前半段有效 JSON。
func strictJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrInvalidJSON
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, ErrTrailingJSON
	}
	return value, nil
}

// validateMetadata 防止使用错误的执行版本解析模型结果。
func validateMetadata(metadata ExecutionMetadata, spec schemaSpec) error {
	if metadata.Provider == "" ||
		metadata.Model == "" ||
		metadata.PromptVersion == "" ||
		metadata.ModelConfigVersion == "" ||
		metadata.SchemaVersion != spec.version {
		return ErrInvalidMetadata
	}
	return nil
}

func validationReason(err error) string {
	switch {
	case errors.Is(err, ErrInvalidJSON):
		return "invalid_json"
	case errors.Is(err, ErrTrailingJSON):
		return "trailing_json"
	case errors.Is(err, ErrSchemaValidation):
		return "schema_validation"
	case errors.Is(err, ErrSemanticValidation):
		return "semantic_validation"
	default:
		return "validation_failed"
	}
}

func validationStageForError(err error) validationStage {
	switch {
	case errors.Is(err, ErrInvalidJSON), errors.Is(err, ErrTrailingJSON):
		return stageJSON
	case errors.Is(err, ErrSchemaValidation):
		return stageSchema
	case errors.Is(err, ErrSemanticValidation):
		return stageSemantic
	default:
		return stageRepair
	}
}
