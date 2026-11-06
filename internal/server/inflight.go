package server

// inflight.go —— 进程级在途请求计数（当前值 + 进程内峰值）
//
// 看板「实时/并发」小卡的数据真源。只统计 /v1/chat/completions 这类业务请求，
// healthz/status/models 等观测端点不计入，避免看板轮询自抬曲线。
// 峰值是进程启动以来最大值，重启归零。

import "sync/atomic"

var (
	inflightCur  atomic.Int64
	inflightPeak atomic.Int64
)

func beginInflight() int64 {
	cur := inflightCur.Add(1)
	for {
		old := inflightPeak.Load()
		if cur <= old || inflightPeak.CompareAndSwap(old, cur) {
			break
		}
	}
	return cur
}

func endInflight() { inflightCur.Add(-1) }

func inflightSnapshot() map[string]any {
	return map[string]any{
		"inflight":      inflightCur.Load(),
		"inflight_peak": inflightPeak.Load(),
		// trae2api 没有实现并发闸门/队列：不设上限，返回 null 而不是编造数字。
		"max_concurrency": nil,
		"max_note":        "无并发闸门",
	}
}
