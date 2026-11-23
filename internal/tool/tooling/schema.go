package tooling

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// validJSONSchema 只验证参数 Schema 是合法 JSON 对象。
//
// Schema 的具体兼容性检查可以在工具注册中心阶段统一编译；本日先保证工具
// 契约不会携带空值、数组、字符串或损坏的 JSON。
func validJSONSchema(raw json.RawMessage) bool {
	_, err := compileJSONSchema(raw)
	return err == nil
}

// compileJSONSchema 将工具参数 Schema 编译成可执行的校验器。
func compileJSONSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, ErrInvalidDefinition
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, ErrInvalidDefinition
	}
	const schemaURL = "inmemory://tooling/parameters.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDefinition, err)
	}
	return compiler.Compile(schemaURL)
}

// validateArguments 使用工具定义中的 Schema 校验本次调用参数。
func validateArguments(schema json.RawMessage, arguments json.RawMessage) error {
	validator, err := compileJSONSchema(schema)
	if err != nil {
		return ErrInvalidDefinition
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(arguments))
	if err != nil {
		return ErrArgumentsInvalid
	}
	if err := validator.Validate(instance); err != nil {
		return ErrArgumentsInvalid
	}
	return nil
}
