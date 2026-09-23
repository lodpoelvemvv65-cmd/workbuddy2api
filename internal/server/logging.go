// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logfmt"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// statsOut 请求流水（表格行）输出目标；nil = 回落到**当前** os.Stdout（docker logs
// 口径不变，且测试的 captureStdout 换 os.Stdout 仍生效）。main 开启 log_file 时经
// SetStatsOutput 接到 io.MultiWriter(os.Stdout, 日志文件)，使流水行与 log 告警
// （断流 WARN 等）落在同一份可跨越容器重建回查的文件里。
var (
	statsOutMu sync.RWMutex
	statsOut   io.Writer
)

// SetStatsOutput 替换请求流水输出目标（nil 回落当前 os.Stdout）。仅启动期 / 测试调用。
func SetStatsOutput(w io.Writer) {
	statsOutMu.Lock()
	statsOut = w
	statsOutMu.Unlock()
}

// statsWriter 读当前请求流水输出目标；未显式设置时动态取 os.Stdout。
func statsWriter() io.Writer {
	statsOutMu.RLock()
	w := statsOut
	statsOutMu.RUnlock()
	if w != nil {
		return w
	}
	return os.Stdout
}

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位
	nick   string // 账号昵称（auth.Auth.Nickname，登录时落盘）；空则只显示 uid8
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	// metrics 采集字段（供 /v1/stats 聚合）：全部来自上游 usage，缺失时保持零值
	// 并由 hasUsage 区分「缺观测」与「显式 0」——与成本账本同一纪律。
	hasUsage  bool
	prompt    int
	cacheHit  int
	cacheMiss int
	cacheWr   int
	credit    float64
	hasCredit bool

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志，并把本次请求记入 metrics 聚合（/v1/stats 数据源）。
//
// 单一埋点：流式 / 非流式 / 各类错误路径最终都汇到此处，故 metrics 天然覆盖全路径，
// 不需要在每个 return 前重复记账（重复记账反而会漏分支或双计）。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	total := time.Since(s.start)
	seq := logChatRow(s.ttfb, total, s.model, s.mode, s.uid, s.nick, s.status, s.toks)
	recordChatMetric(s, total)
	// 最近流水环形缓冲（/v1/logs 数据源）。与表格日志同点写入，seq 共用——
	// 页面上的 #序号与日志文件逐条对齐，排障时可互相指认。
	appendRequestLog(s, total, seq)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br        *bufio.Reader
	start     time.Time
	ttfb      time.Duration
	seen      bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage  bool // 末帧是否带 usage
	hasCredit bool // 是否出现过带 credit 的 usage（缺失≠0，见 Credit() 注释）
	tokens    int
	credit    float64 // 末帧 usage.credit（本次真实扣费，供成本账本）
	prompt    int     // 末帧 usage.prompt_tokens（与 completion 合计折算单价）
	cacheHit  int     // 末帧 usage.prompt_cache_hit_tokens（供 /v1/stats）
	cacheMiss int     // 末帧 usage.prompt_cache_miss_tokens
	cacheWr   int     // 末帧 usage.prompt_cache_write_tokens
	pend      []byte  // 已读未返回的行缓存

	// sawDone 上游显式发过 data: [DONE]（正常收尾）。
	sawDone bool
	// sawEOF 上游 body 已被读到 EOF。
	// 与 sawDone 组合出「上游断流」的**直接证据**：读到 EOF 却从未见 [DONE]。
	// 之所以必须在此单独观测：upstream.StreamHint 会在收尾时无条件补发一个
	// [DONE]（保证客户端能正常收尾），且它恒返回 nil（见 TestStreamDoneFallback）
	// ——断流事实在透传层被有意吞掉，不记这行就只能靠"usage 缺失"间接猜测，
	// 与"上游本就不给 usage"混为一谈，出问题时无从取证。
	sawEOF bool
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// SawDone 上游是否显式发过 data: [DONE]（正常收尾标记）。
func (s *chatStatsReader) SawDone() bool { return s.sawDone }

// SawEOF 上游 body 是否读到 EOF（false = 读取被提前中断，如客户端断连）。
func (s *chatStatsReader) SawEOF() bool { return s.sawEOF }

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// Credit 返回末帧 usage.credit（本次真实扣费）。ok=true 要求 usage 存在**且** credit
// 字段显式出现——字段缺失时 ok=false（缺失≠0：不能把"缺观测"当"0 成本"写入账本，
// 否则收费的号可能被误判 tier0 免费层）。显式 credit:0 仍是合法免费观测（ok=true）。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasUsage && s.hasCredit }

// TotalTokens 返回本次请求总 token 数（prompt + completion），供成本单价折算。
func (s *chatStatsReader) TotalTokens() int { return s.prompt + s.tokens }

// PromptTokens 返回末帧 usage.prompt_tokens（供 /v1/stats 输入侧统计）。
func (s *chatStatsReader) PromptTokens() int { return s.prompt }

// CacheTokens 返回缓存三段计数（命中 / 未命中 / 写入），供 /v1/stats 的命中率聚合。
func (s *chatStatsReader) CacheTokens() (hit, miss, write int) {
	return s.cacheHit, s.cacheMiss, s.cacheWr
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		s.sawDone = true
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Usage *struct {
			CompletionTokens int      `json:"completion_tokens"`
			PromptTokens     int      `json:"prompt_tokens"`
			Credit           *float64 `json:"credit"` // 指针区分「缺失」与「显式 0」
			// 缓存三段（上游实测字段名，见 /v1/stats 的 cache_* 口径）。
			PromptCacheHitTokens   int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens  int `json:"prompt_cache_miss_tokens"`
			PromptCacheWriteTokens int `json:"prompt_cache_write_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
	s.prompt = chunk.Usage.PromptTokens
	s.cacheHit = chunk.Usage.PromptCacheHitTokens
	s.cacheMiss = chunk.Usage.PromptCacheMissTokens
	s.cacheWr = chunk.Usage.PromptCacheWriteTokens
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		if err == io.EOF {
			s.sawEOF = true
		}
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	if err == io.EOF {
		s.sawEOF = true
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// usageCreditTotal 从聚合响应提取本次真实扣费与总 token 数（供成本账本）。
// ok=false 表示 usage 缺失或字段类型不符——此时不记录观测，避免污染账本。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	c, hasCredit := u["credit"].(float64)
	pt, hasPrompt := u["prompt_tokens"].(float64)
	ct, hasCompletion := u["completion_tokens"].(float64)
	if !hasCredit || (!hasPrompt && !hasCompletion) {
		return 0, 0, false
	}
	return c, int(pt) + int(ct), true
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
//
// 保留本函数是因为 logging_test.go 直接断言它；实现委托 logfmt.UID8，避免
// "截 8 位" 的规则在 server 与 logfmt 两处各写一份而走样。
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

// 请求流水行的固定列宽（显示列宽，非字节）。取固定宽度而不是让内容自然长度撑开，
// 是为了让 stdout 里成百上千行能竖着扫——否则模型名长短不一、中文昵称按字节补空格
// 错位，根本没法用肉眼对齐着一列列看（这正是上一版 11 字节硬截断要解决的问题）。
const (
	// chatModelWidth 覆盖 realm 前缀 + 最长模型名："global:" (7) + "deepseek-v4.1-flash" (19) = 26。
	// 旧的 11 字节截断会把 "cn:deepseek-v4-flash" 切成 "cn:deepseek"，让人误以为是另一个模型。
	chatModelWidth = 26
	// chatAcctWidth 容纳 "昵称(uid8)"：中文昵称按 2 列/字算，5 字中文 + "(xxxxxxxx)" = 20 列。
	chatAcctWidth = 22
	chatTTFBWidth = 8
	chatTokWidth  = 6
	chatRateWidth = 11 // 形如 "183.6tok/s"
)

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀），
// 并返回该行的进程级序号（供最近流水缓冲对齐；chatLogEnabled=false 时返回 0）。
//
// 参数：
//   - model：模型名（含 realm 前缀），超 chatModelWidth 截断（模型名是 ASCII，字节截即列宽）；
//   - uid/nick：完整 uid 与账号昵称，经 logfmt.Label 拼成 "昵称(uid8)" 展示——只有
//     uid8 时人眼无法判断是哪个号，要辨认必须再查 auths/，排障多一跳；
//   - toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int) int64 {
	if !chatLogEnabled {
		return 0
	}
	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// 账号标签只补不截：超宽时宁可让该行变宽，也不丢昵称信息（昵称是排查的主线索）。
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1ftok/s", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0tok/s"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(statsWriter(), "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth),
		logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(tokpsField, chatRateWidth),
		total.Seconds(),
	)
	return seq
}
