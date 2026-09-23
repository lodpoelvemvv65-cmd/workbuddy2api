// reqlog.go 最近请求流水（/v1/logs 数据源）。
//
// 为什么要它：请求流水此前只写 stdout + 日志文件（logging.go），是同机、逐行文本的
// 形态——独立面板（cmd/dashboard）无法远程消费，也拿不到稳定结构。本文件在网关侧
// 留一份**有界内存环形缓冲**：字段与流水行同构、按请求出口单点写入，供页面轮询。
//
// 设计纪律（与 metrics.go 一致）：
//   - **单一埋点**：唯一写入口是 appendRequestLog，由 chatStat.done() 调用，流式 /
//     非流式 / 错误 / 两套协议垫片（messages / responses）全部覆盖；
//   - **有界内存**：固定容量环形缓冲，满则覆盖最旧一条，绝不无界增长；
//   - **不依赖日志文件**：log_file 为空（不落盘）时本缓冲照常工作；
//   - **只读快照**：读取端拿到的是一份独立切片拷贝，后续写入不会影响它。
package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// requestLogCapacity 环形缓冲容量（条）。按人眼看日志的窗口取千级：太小人翻历史
// 就断档，太大徒占内存（每条百来字节，1000 条约百 KB 级）。
const requestLogCapacity = 1000

// RequestLogEntry 单条请求流水（字段与 logging.go 的表格行同构，供面板直接渲染）。
//
// 「缺失≠0」在两个 has_* 布尔上体现：has_usage=false 时 token / 缓存字段无观测，
// 页面应显示占位符而不是 0；completion_tokens=-1 是 usage 缺失哨兵（沿用 chatStat
// 的口径），与 has_usage 互相印证。
type RequestLogEntry struct {
	Seq      int64     `json:"seq"`
	Time     time.Time `json:"time"`
	Model    string    `json:"model"`
	Mode     string    `json:"mode"` // "stream" | "sync"
	Status   int       `json:"status"`
	UID      string    `json:"uid,omitempty"`
	Nickname string    `json:"nickname,omitempty"`

	TTFBMS     int64 `json:"ttfb_ms,omitempty"` // 0 = 无观测（非流式）
	DurationMS int64 `json:"duration_ms"`

	CompletionTokens int     `json:"completion_tokens"` // -1 = usage 缺失
	PromptTokens     int     `json:"prompt_tokens"`
	TokensPerSec     float64 `json:"tokens_per_sec"`

	CacheHitTokens   int `json:"cache_hit_tokens"`
	CacheMissTokens  int `json:"cache_miss_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`

	Credit    float64 `json:"credit"`
	HasUsage  bool    `json:"has_usage"`
	HasCredit bool    `json:"has_credit"`
}

// RequestLogSnapshot /v1/logs 的响应载荷。entries 按时间**倒序**（最新在前）。
type RequestLogSnapshot struct {
	Enabled  bool              `json:"enabled"`
	Capacity int               `json:"capacity"`
	Count    int               `json:"count"` // 当前缓冲内的条数（≤ capacity）
	Total    int64             `json:"total"` // 进程内累计写入条数（可 > capacity）
	Entries  []RequestLogEntry `json:"entries"`
}

// requestLog 环形缓冲。用「固定切片 + 写指针 + 计数」实现，覆盖最旧无需搬移。
var requestLog struct {
	mu    sync.Mutex
	buf   []RequestLogEntry
	next  int   // 下一条写入位置
	count int   // 当前有效条数（≤ len(buf)）
	total int64 // 累计写入（含已被覆盖的）
}

// appendRequestLog 把一次请求出口的观测写入环形缓冲。由 chatStat.done() 调用。
//
// seq 与表格日志共用（由 logChatRow 分配）：页面「最近日志」与文件里看到的 #序号
// 逐条对齐，排障时能互相指认。total 为端到端耗时。
func appendRequestLog(s *chatStat, total time.Duration, seq int64) {
	e := RequestLogEntry{
		Seq:        seq,
		Time:       time.Now(),
		Model:      s.model,
		Mode:       s.mode,
		Status:     s.status,
		UID:        s.uid,
		Nickname:   s.nick,
		DurationMS: total.Milliseconds(),
		HasUsage:   s.hasUsage,
		HasCredit:  s.hasCredit,
	}
	if s.ttfb > 0 {
		e.TTFBMS = s.ttfb.Milliseconds()
	}
	// toks<0 是「usage 缺失」哨兵：原样保留 -1，不与观测到的 0 混淆。
	if s.toks >= 0 {
		e.CompletionTokens = s.toks
		if total > 0 {
			e.TokensPerSec = float64(s.toks) / total.Seconds()
		}
	} else {
		e.CompletionTokens = -1
	}
	if s.hasUsage {
		e.PromptTokens = s.prompt
		e.CacheHitTokens = s.cacheHit
		e.CacheMissTokens = s.cacheMiss
		e.CacheWriteTokens = s.cacheWr
	}
	if s.hasCredit {
		e.Credit = s.credit
	}

	requestLog.mu.Lock()
	defer requestLog.mu.Unlock()
	if requestLog.buf == nil {
		requestLog.buf = make([]RequestLogEntry, requestLogCapacity)
	}
	requestLog.buf[requestLog.next] = e
	requestLog.next = (requestLog.next + 1) % len(requestLog.buf)
	if requestLog.count < len(requestLog.buf) {
		requestLog.count++
	}
	requestLog.total++
}

// RecentRequestLogs 返回最近 limit 条流水，最新在前；limit<=0 视为取全部。
// 返回的是独立切片拷贝，调用方可安全持有。
func RecentRequestLogs(limit int) []RequestLogEntry {
	requestLog.mu.Lock()
	defer requestLog.mu.Unlock()

	count := requestLog.count
	if count == 0 {
		return nil
	}
	if limit <= 0 || limit > count {
		limit = count
	}
	out := make([]RequestLogEntry, 0, limit)
	// next 指向「下一条要写」的位置，即最旧一条之后；从 next-1 往回走即最新在前。
	n := len(requestLog.buf)
	for i := 0; i < limit; i++ {
		idx := (requestLog.next - 1 - i + n*2) % n
		out = append(out, requestLog.buf[idx])
	}
	return out
}

// RequestLogStats 返回 (容量, 当前条数, 累计写入)，供 /v1/logs 信封。
func RequestLogStats() (capacity, count int, total int64) {
	requestLog.mu.Lock()
	defer requestLog.mu.Unlock()
	return requestLogCapacity, requestLog.count, requestLog.total
}

// resetRequestLog 清空缓冲（仅测试使用）。
func resetRequestLog() {
	requestLog.mu.Lock()
	defer requestLog.mu.Unlock()
	requestLog.buf = nil
	requestLog.next = 0
	requestLog.count = 0
	requestLog.total = 0
}

// logs 处理 GET /v1/logs：返回最近请求流水（社区面板 / cmd/dashboard 数据源）。
//
// 查询参数 limit 限定返回条数（默认 100，上限为环形缓冲容量）；非法值回落默认，
// 不报错——面板轮询时一个坏参数不该让整页拿不到数据。
func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	limit := parseLogLimit(r.URL.Query().Get("limit"))
	capacity, count, total := RequestLogStats()
	writeJSON(w, http.StatusOK, RequestLogSnapshot{
		Enabled:  true,
		Capacity: capacity,
		Count:    count,
		Total:    total,
		Entries:  RecentRequestLogs(limit),
	})
}

// defaultLogLimit / maxLogLimit /v1/logs 的默认与上限条数。
const (
	defaultLogLimit = 100
	maxLogLimit     = requestLogCapacity
)

// parseLogLimit 解析 limit 查询参数：空/非法 → 默认；超上限 → 截到上限。
func parseLogLimit(raw string) int {
	if raw == "" {
		return defaultLogLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultLogLimit
	}
	if n > maxLogLimit {
		return maxLogLimit
	}
	return n
}
