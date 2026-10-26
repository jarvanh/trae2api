package server

import "trae2api/internal/upstream"

// modelContextLength 返回上游提供的真实上下文窗口。
//
// ok=false 表示未知；调用方应省略 context_length，禁止再伪造默认值。
func modelContextLength(mi upstream.ModelInfo) (int64, bool) {
	if mi.ContextWindow > 0 {
		return mi.ContextWindow, true
	}
	return 0, false
}
