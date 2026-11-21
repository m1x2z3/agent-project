package readtools

import "errors"

var (
	// ErrProjectNotFound 表示请求的项目不存在。
	ErrProjectNotFound = errors.New("project not found")
	// ErrDocumentStoreTimeout 表示文档数据源在规定时间内没有完成查询。
	ErrDocumentStoreTimeout = errors.New("document store timeout")
	// ErrDocumentStoreInternal 表示文档数据源发生未分类的内部错误。
	ErrDocumentStoreInternal = errors.New("document store internal error")
)
