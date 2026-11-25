// Package clone 提供不携带业务语义的可变数据复制辅助函数。
package clone

// Map 复制一个 Map 的键和值。
//
// 该函数只复制 Map 容器本身。若 Value 内部还包含指针、切片或嵌套
// Map，调用方仍需在领域层继续处理这些嵌套数据。
func Map[K comparable, V any](source map[K]V) map[K]V {
	if source == nil {
		return nil
	}
	target := make(map[K]V, len(source))
	for key, value := range source {
		target[key] = value
	}
	return target
}
