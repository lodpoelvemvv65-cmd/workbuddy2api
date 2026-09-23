package server

import (
	"testing"
	"time"
)

// BenchmarkAppendRequestLog 衡量新增的**每个请求**写入成本（环形缓冲追加）。
// 与 BenchmarkRecordChatMetric 对照，即可看出本功能在既有记账之外的增量占比。
func BenchmarkAppendRequestLog(b *testing.B) {
	resetRequestLog()
	st := mkStat("deepseek-v4.1-flash", 200, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		appendRequestLog(st, 1500*time.Millisecond, int64(i))
	}
}

// BenchmarkRecordChatMetric 既有 /v1/stats 聚合的每请求成本（基线对照）。
func BenchmarkRecordChatMetric(b *testing.B) {
	st := mkStat("deepseek-v4.1-flash", 200, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recordChatMetric(st, 1500*time.Millisecond)
	}
}

// BenchmarkRecordChatMetricParallel 既有聚合的并发成本（基线对照）。
func BenchmarkRecordChatMetricParallel(b *testing.B) {
	st := mkStat("deepseek-v4.1-flash", 200, 128)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordChatMetric(st, 1500*time.Millisecond)
		}
	})
}

// BenchmarkAppendRequestLogParallel 测并发追加的锁竞争成本（全局互斥锁是唯一
// 潜在热点；与上面的串行值对照即可看出争用带来的放大）。
func BenchmarkAppendRequestLogParallel(b *testing.B) {
	resetRequestLog()
	st := mkStat("deepseek-v4.1-flash", 200, 128)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var seq int64
		for pb.Next() {
			appendRequestLog(st, 1500*time.Millisecond, seq)
			seq++
		}
	})
}

// BenchmarkRecentRequestLogs100 面板轮询一次 /v1/logs?limit=100 的读取成本
// （面板每 5s 一次，即 0.2 QPS；此处只为量化单次开销）。
func BenchmarkRecentRequestLogs100(b *testing.B) {
	resetRequestLog()
	st := mkStat("deepseek-v4.1-flash", 200, 128)
	for i := 0; i < requestLogCapacity; i++ {
		appendRequestLog(st, 1500*time.Millisecond, int64(i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RecentRequestLogs(100)
	}
}
