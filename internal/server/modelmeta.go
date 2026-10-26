package server

import "trae2api/internal/upstream"

// fallbackContextLength 上游未返回上下文窗口时的保守兜底值。
//
// 仅用于元数据缺失，不代表任何模型的真实窗口；正常情况下
// upstream.FetchModels 会解析 context_window_tokens / prompt_max_tokens。
const fallbackContextLength int64 = 131072

// modelContextLength 统一 /v1/models 与管理台的 context_length 口径。
func modelContextLength(mi upstream.ModelInfo) int64 {
	if mi.ContextWindow > 0 {
		return mi.ContextWindow
	}
	return fallbackContextLength
}
