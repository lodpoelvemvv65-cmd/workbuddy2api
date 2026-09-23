// truncation_metrics.go 上游断流（EOF 且无 finish_reason）的可观测计数。
//
// 为什么需要：断流在网关里被刻意「诚实化」——/v1/messages 发 error 事件、
// /v1/responses 报 incomplete——但如果没有计数，运维只能靠日志逐条翻。且 handler
// 里 "EOF before [DONE]" 的 WARN 依赖**干净 EOF**（SawEOF）：TCP RST 类中断读不到
// EOF，会漏报，且它只覆盖流式路径。本计数在协议适配层触发（流式 failInterrupted /
// 非流式 TruncatedKey），是权威信号，流式与非流式两条路都覆盖。
//
// 只对「缺 finish_reason 的上游中断」计数；finish_reason=length（max_tokens 截断）
// 是模型正常收尾，不计入——两者对客户端的语义不同，混在一起会让趋势失真。
//
// 进程内累计、重启清零（与 /v1/stats 的 metricsStore 同纪律）。
package server

import (
	"log"
	"sync/atomic"
)

// upstreamTruncation 上游断流计数，按协议维度分桶。
var upstreamTruncation struct {
	messages  atomic.Int64
	responses atomic.Int64
}

// noteUpstreamTruncation 记一次上游断流并留一行 WARN。
//   - protocol："messages"（/v1/messages）/ "responses"（/v1/responses）；
//   - model：出站模型名（与请求流水行对照排障）；
//   - streaming：true=流式路径（failInterrupted），false=非流式（TruncatedKey）。
func noteUpstreamTruncation(protocol, model string, streaming bool) {
	switch protocol {
	case "messages":
		upstreamTruncation.messages.Add(1)
	case "responses":
		upstreamTruncation.responses.Add(1)
	}
	mode := "stream"
	if !streaming {
		mode = "non-stream"
	}
	log.Printf("WARN: [server] upstream truncated protocol=%s model=%s mode=%s (EOF without finish_reason)",
		protocol, model, mode)
}

// upstreamTruncationSnapshot 读计数快照（/status 观测）。
func upstreamTruncationSnapshot() map[string]int64 {
	return map[string]int64{
		"messages":  upstreamTruncation.messages.Load(),
		"responses": upstreamTruncation.responses.Load(),
	}
}
