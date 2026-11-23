package registry

import (
	"sort"
	"strings"
	"sync"

	"agent-project/internal/tool/tooling"
)

// Registry 是进程内的工具注册中心。
//
// 工具按名称和版本分层保存：
//
//	工具名称 -> 工具版本 -> RegisteredTool
//
// versions 保存工具定义和实例，mu 保护注册表在并发注册、查询时的一致性。
// 注册中心只负责工具发现，不负责调用工具。
type Registry struct {
	mu       sync.RWMutex
	versions map[string]map[string]RegisteredTool
}

// NewRegistry 创建一个空的内存工具注册中心。
func NewRegistry() *Registry {
	return &Registry{
		versions: make(map[string]map[string]RegisteredTool),
	}
}

// Register 校验并保存一个工具实例。
//
// 注册时会先读取工具定义并执行统一契约校验，再复制定义中的可变字段，
// 最后在写锁保护下写入名称和版本索引。注册中心只保存工具，不调用
// Tool.Execute。
func (r *Registry) Register(tool tooling.Tool) error {
	if r == nil || tool == nil {
		return ErrInvalidRegistryTool
	}

	definition := tool.Definition()
	if err := definition.Validate(); err != nil {
		return ErrInvalidRegistryTool
	}

	registered := RegisteredTool{
		Definition: cloneToolDefinition(definition),
		Tool:       tool,
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.versions == nil {
		r.versions = make(map[string]map[string]RegisteredTool)
	}
	toolVersions, exists := r.versions[definition.Name]
	if !exists {
		toolVersions = make(map[string]RegisteredTool)
		r.versions[definition.Name] = toolVersions
	}
	if _, exists := toolVersions[definition.Version]; exists {
		return ErrDuplicateTool
	}
	toolVersions[definition.Version] = registered
	return nil
}

// Resolve 按工具名称和版本解析一个可用工具。
//
// 名称不存在和版本不存在使用不同错误，便于调用方区分发现失败原因。
// 已废弃版本仍保留在注册中心中，但不能通过 Resolve 作为可用工具返回。
// 返回值中的定义会再次复制，避免调用方修改查询结果时污染内部快照。
func (r *Registry) Resolve(name, version string) (RegisteredTool, error) {
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(version)
	if r == nil || name == "" {
		return RegisteredTool{}, ErrToolNotFound
	}
	if version == "" {
		return RegisteredTool{}, ErrToolVersionNotFound
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	toolVersions, exists := r.versions[name]
	if !exists {
		return RegisteredTool{}, ErrToolNotFound
	}
	registered, exists := toolVersions[version]
	if !exists {
		return RegisteredTool{}, ErrToolVersionNotFound
	}
	if registered.Definition.Deprecated {
		return RegisteredTool{}, ErrToolDeprecated
	}

	registered.Definition = cloneToolDefinition(registered.Definition)
	return registered, nil
}

// ListAll 返回注册中心中的全部工具定义，包括已经废弃的版本。
//
// 返回值中的每个定义都是独立副本，调用方可以安全修改。由于注册表使用
// Map 保存数据，返回前必须显式排序，保证同样的注册内容始终产生稳定顺序。
func (r *Registry) ListAll() []tooling.ToolDefinition {
	return r.listDefinitions(func(tooling.ToolDefinition) bool {
		return true
	})
}

// ListAvailable 返回当前可以被默认发现的工具定义。
//
// 已废弃版本仍然保存在注册中心中，便于历史查询和显式处理；但默认发现
// 结果不应继续暴露这些版本，因此这里会过滤 Deprecated=true 的定义。
// 返回值仍然是深拷贝，并按名称、版本和能力标签稳定排序。
func (r *Registry) ListAvailable() []tooling.ToolDefinition {
	return r.listDefinitions(func(definition tooling.ToolDefinition) bool {
		return !definition.Deprecated
	})
}

// FindByCapability 根据能力标签查找可用工具定义。
//
// 能力标签按完整字符串匹配，查询前会去除首尾空格。已废弃版本不会出现在
// 结果中；空标签没有明确的查询语义，因此返回空切片。返回值是深拷贝，并
// 按工具名称、版本和能力标签稳定排序。
func (r *Registry) FindByCapability(tag string) []tooling.ToolDefinition {
	tag = strings.TrimSpace(tag)
	if r == nil || tag == "" {
		return []tooling.ToolDefinition{}
	}

	return r.listDefinitions(func(definition tooling.ToolDefinition) bool {
		if definition.Deprecated {
			return false
		}
		for _, capability := range definition.CapabilityTags {
			if strings.TrimSpace(capability) == tag {
				return true
			}
		}
		return false
	})
}

// listDefinitions 统一处理工具定义的遍历、过滤、深拷贝和稳定排序。
//
// 调用方只提供过滤条件，注册中心统一负责并发读取和返回值隔离，避免
// ListAll、ListAvailable 和 FindByCapability 在这些基础逻辑上重复实现。
func (r *Registry) listDefinitions(
	match func(tooling.ToolDefinition) bool,
) []tooling.ToolDefinition {
	if r == nil {
		return []tooling.ToolDefinition{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	definitions := make([]tooling.ToolDefinition, 0)
	for _, toolVersions := range r.versions {
		for _, registered := range toolVersions {
			if !match(registered.Definition) {
				continue
			}
			definitions = append(definitions, cloneToolDefinition(registered.Definition))
		}
	}

	sort.Slice(definitions, func(i, j int) bool {
		if definitions[i].Name != definitions[j].Name {
			return definitions[i].Name < definitions[j].Name
		}
		if definitions[i].Version != definitions[j].Version {
			return definitions[i].Version < definitions[j].Version
		}
		return strings.Join(definitions[i].CapabilityTags, "\x00") <
			strings.Join(definitions[j].CapabilityTags, "\x00")
	})
	return definitions
}

// Versions 返回指定工具已经注册的全部版本。
//
// 版本只按字符串排序，不推断语义化版本的大小，也不自动选择“最新”版本。
// 调用方必须显式选择需要的版本；未知工具、空名称或空注册中心返回空切片。
func (r *Registry) Versions(name string) []string {
	name = strings.TrimSpace(name)
	if r == nil || name == "" {
		return []string{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	toolVersions, exists := r.versions[name]
	if !exists {
		return []string{}
	}

	versions := make([]string, 0, len(toolVersions))
	for version := range toolVersions {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	return versions
}
