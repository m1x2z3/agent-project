package structuredoutput

import (
	"encoding/json"
	"fmt"

	"github.com/eino-contrib/jsonschema"
)

type schemaSpec struct {
	kind    OutputKind
	version string
	value   any
}

// schemaFor 将每个对外解析入口映射到稳定的 Schema 版本和对应 Go 类型。
func schemaFor(kind OutputKind) (schemaSpec, error) {
	switch kind {
	case OutputTaskAnalysis:
		return schemaSpec{
			kind:    kind,
			version: TaskAnalysisSchemaVersion,
			value:   TaskAnalysis{},
		}, nil
	case OutputToolDecision:
		return schemaSpec{
			kind:    kind,
			version: ToolDecisionSchemaVersion,
			value:   ToolDecision{},
		}, nil
	case OutputFinalAnswer:
		return schemaSpec{
			kind:    kind,
			version: FinalAnswerSchemaVersion,
			value:   FinalAnswer{},
		}, nil
	default:
		return schemaSpec{}, fmt.Errorf("%w: %q", ErrUnsupportedOutput, kind)
	}
}

// marshalSchema 根据 Go 结构定义生成封闭的 JSON Schema 文档。
func marshalSchema(spec schemaSpec) ([]byte, error) {
	reflector := &jsonschema.Reflector{
		Anonymous:                  true,
		AllowAdditionalProperties:  false,
		RequiredFromJSONSchemaTags: true,
	}
	schema := reflector.Reflect(spec.value)
	return json.Marshal(schema)
}
