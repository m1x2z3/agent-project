package agent

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	"agent-project/internal/configregistry"
)

func validateRunInput(input RunInput) error {
	if strings.TrimSpace(input.RequestID) == "" ||
		strings.TrimSpace(input.RunID) == "" ||
		strings.TrimSpace(input.Query) == "" {
		return ErrInvalidRunInput
	}
	return nil
}

func promptFromSnapshot(snapshot configregistry.ConfigSnapshot, name string) (configregistry.PromptVersion, error) {
	prompt, ok := snapshot.Prompts[name]
	if !ok || strings.TrimSpace(prompt.Template) == "" || strings.TrimSpace(prompt.Version) == "" {
		return configregistry.PromptVersion{}, ErrPromptNotFound
	}
	return prompt, nil
}

func routeFromSnapshot(snapshot configregistry.ConfigSnapshot, task string) (configregistry.ModelRouteVersion, error) {
	// 路由也必须从绑定快照读取，确保任务使用的 Provider、Model 和参数可追溯。
	route, ok := snapshot.Routes[task]
	if !ok || strings.TrimSpace(route.Provider) == "" || strings.TrimSpace(route.Model) == "" {
		return configregistry.ModelRouteVersion{}, ErrRouteNotFound
	}
	return route, nil
}

func schemaFromSnapshot(snapshot configregistry.ConfigSnapshot, kind string) (configregistry.SchemaVersion, error) {
	schema, ok := snapshot.Schemas[kind]
	if !ok || strings.TrimSpace(schema.Kind) == "" || strings.TrimSpace(schema.Version) == "" {
		return configregistry.SchemaVersion{}, ErrSchemaNotFound
	}
	return schema, nil
}

func resolveReferences(
	snapshot configregistry.ConfigSnapshot,
	promptName string,
	schemaKind string,
	task string,
) (configregistry.VersionReferences, error) {
	// 先分别确认 Prompt、Schema、Route 存在，再统一生成版本元数据，
	// 让结构化输出报告能够记录本次解析使用的完整版本组合。
	if _, err := promptFromSnapshot(snapshot, promptName); err != nil {
		return configregistry.VersionReferences{}, err
	}
	if _, err := schemaFromSnapshot(snapshot, schemaKind); err != nil {
		return configregistry.VersionReferences{}, err
	}
	if _, err := routeFromSnapshot(snapshot, task); err != nil {
		return configregistry.VersionReferences{}, err
	}
	return snapshot.ResolveVersionReferences(promptName, schemaKind, task)
}

func renderPrompt(source string, data map[string]any) (string, error) {
	// missingkey=error 会把模板变量拼写错误或配置遗漏立即变成错误，
	// 避免模型收到缺少上下文的半成品 Prompt。
	tmpl, err := template.New("runtime-prompt").Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("%w: template parse failed", ErrPromptRenderFailed)
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return "", fmt.Errorf("%w: template execution failed", ErrPromptRenderFailed)
	}
	if strings.TrimSpace(output.String()) == "" {
		return "", fmt.Errorf("%w: rendered prompt is empty", ErrPromptRenderFailed)
	}
	return output.String(), nil
}

func executionFromContext(ctx context.Context) *runtimeExecution {
	execution, _ := ctx.Value(executionContextKey{}).(*runtimeExecution)
	return execution
}

func (r *Runtime) selectorCandidateVersion() string {
	if r == nil || r.selector == nil {
		return ""
	}
	return r.selector.Policy().CandidateVersion
}

func float64Pointer(value float64) *float64 {
	return &value
}

func maxTokensPointer(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}
