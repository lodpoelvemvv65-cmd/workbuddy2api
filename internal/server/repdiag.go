// repdiag.go 退化重复（模型陷入思维链/正文循环）诊断。
//
// 背景：deepseek 系在 global 域被强制 high 思考档（见 upstream/effort_catalog.go：
// global 的 deepseek-v4.1-flash 只认 ["high"]，客户端请求的 medium 被 floored 到
// high），模型偶尔陷入自我复读（如 "Let me run. Let me do it. Let me go. ..." 循环），
// 一路跑到 token 上限（日志里 tok=65536 / total=154s 那种）。网关此前对这种退化
// **零可观测**——请求流水只有一行 tok=65536，看不到到底在复读什么、复读多少轮。
//
// 本文件在流式透传时**旁路**累积思维链（reasoning_content）与正文（content），
// 检测「同一单元连续重复 ≥ N 次」，命中即打 WARN；可选 WB2A_DUMP_RESP 落盘原文
// 供离线分析（连请求体一起落，方便复现）。
//
// 设计纪律（与 reqlog.go 一致）：
//   - **纯观测**：只读累积、绝不改写/延迟透传字节；
//   - **有界内存**：每路文本只保留最近 512KB，检测只看尾部 2KB 窗口；
//   - **零配置可用**：检测恒开（每 512 字节才扫一次，成本可忽略），落盘才需开关；
//   - **单一埋点**：唯一喂入口是 chatStatsReader.parseSSELine（流式路径）。
package server

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	// repKeepMax 每路（reasoning / content）保留的最大字节数；超 repKeepMax*2 裁剪到
	// repKeepMax。够看循环长什么样（复读单元通常几十字节，几十万字节足够）又不失控。
	repKeepMax = 512 << 10
	// repDetectWindow 重复检测只看尾部窗口，避免长文本上 O(n²) 扫描。
	repDetectWindow = 2048
	// repCheckEvery 每累积这么多新字节才检测一次（降采样，成本可忽略）。
	repCheckEvery = 512
	// repMinUnit / repMinRepeats 判定阈值：单元 ≥8 字节且连续 ≥4 次才算退化。
	// 8 字节下限过滤 "    " 这类空白噪声；4 次下限过滤正常写作里的偶发重复。
	repMinUnit    = 8
	repMinRepeats = 4
	// repMaxUnit 单元搜索上限：比这更长的"循环"按更短周期识别即可（更具体）。
	repMaxUnit = 160
	// dumpSentinelPath 落盘开关的哨兵文件（容器内路径；宿主为 ./data/dump_resp.on）。
	// 存在即开启落盘——运行中 touch/rm 即可切换，**无需重启**（env 只在启动时读取）。
	dumpSentinelPath = "/app/data/dump_resp.on"
	// dumpDefaultDir 默认落盘目录（与既有 WB2A_DUMP_REQ 同口径）。
	dumpDefaultDir = "/app/data"
)

// repBuf 有界文本缓冲：追加 + 超限裁剪（批量裁剪，避免每帧都搬移）。
type repBuf struct {
	data      []byte
	lastCheck int // 上次检测时的长度（节流用）
}

func (b *repBuf) write(s string) {
	if s == "" {
		return
	}
	b.data = append(b.data, s...)
	if len(b.data) > repKeepMax*2 {
		keep := append([]byte(nil), b.data[len(b.data)-repKeepMax:]...)
		b.data = keep
		b.lastCheck = 0 // 裁剪后长度口径变了，重置节流基线
	}
}

func (b *repBuf) len() int { return len(b.data) }

// detectRepeat 在 data 尾部窗口里寻找「同一子串连续重复 ≥ repMinRepeats 次」的单元。
// 返回重复单元、连续次数与是否命中。取「重复次数最多」的周期（即最基础的循环单元）；
// 次数相同取更短周期。
func detectRepeat(data []byte) (unit string, count int, ok bool) {
	n := len(data)
	if n < repMinUnit*repMinRepeats {
		return "", 0, false
	}
	win := data
	if n > repDetectWindow {
		win = data[n-repDetectWindow:]
	}
	m := len(win)
	maxP := m / repMinRepeats
	if maxP > repMaxUnit {
		maxP = repMaxUnit
	}
	bestP, bestCnt := 0, 0
	for p := repMinUnit; p <= maxP; p++ {
		u := win[m-p:]
		if len(bytes.TrimSpace(u)) == 0 {
			continue // 纯空白单元不是退化信号
		}
		if uniformUnit(u) {
			continue // 单一字符重复（─── / ======== / .....）是分隔线，不是退化复读
		}
		cnt := 1
		for j := m - p; j-p >= 0; j -= p {
			if bytes.Equal(win[j-p:j], u) {
				cnt++
			} else {
				break
			}
		}
		// 取重复次数最多者；并列取更短周期（更基础的循环单元）。
		if cnt >= repMinRepeats && (cnt > bestCnt || (cnt == bestCnt && (bestP == 0 || p < bestP))) {
			bestP, bestCnt = p, cnt
		}
	}
	if bestP == 0 {
		return "", 0, false
	}
	return string(win[m-bestP:]), bestCnt, true
}

// uniformUnit 判断单元去掉空白后是否只由**同一个字符**组成。
//
// 为什么需要：模型画分隔线（`──────`、`========`、`........`）时，算法会把它
// 当成"3 字节单元 × 10 次"这类重复而误报（实测 `───` 一个字符占 3 字节，
// 30 个横线就凑够 ≥8 字节单元 × ≥4 次）。这类单字符重复没有退化语义，排除。
func uniformUnit(u []byte) bool {
	var first rune
	seen := false
	for _, r := range string(u) {
		if unicode.IsSpace(r) {
			continue
		}
		if !seen {
			first = r
			seen = true
			continue
		}
		if r != first {
			return false
		}
	}
	return seen // 全是空白（seen=false）也视为 uniform，一并排除
}

// repDiag 单次流式请求的重复诊断累积器。零值可用。
type repDiag struct {
	reasoning repBuf
	content   repBuf

	// hit 首个命中即锁定（只报一次），此后继续累积文本（供落盘），不再检测。
	hit        bool
	hitKind    string // "reasoning" | "content"
	hitUnit    string
	hitCount   int
	hitReason  int // 命中时 reasoning 总字节
	hitContent int // 命中时 content 总字节
}

func (d *repDiag) feedReasoning(s string) { d.feed(&d.reasoning, s, "reasoning") }
func (d *repDiag) feedContent(s string)   { d.feed(&d.content, s, "content") }

func (d *repDiag) feed(b *repBuf, s, kind string) {
	b.write(s)
	if d.hit || s == "" {
		return
	}
	if b.len()-b.lastCheck < repCheckEvery {
		return
	}
	d.check(b, kind)
}

// check 对单路缓冲做一次重复检测（节流已过或流已结束）。
func (d *repDiag) check(b *repBuf, kind string) {
	if d.hit {
		return
	}
	b.lastCheck = b.len()
	if unit, cnt, ok := detectRepeat(b.data); ok {
		d.hit = true
		d.hitKind = kind
		d.hitUnit = unit
		d.hitCount = cnt
		d.hitReason = d.reasoning.len()
		d.hitContent = d.content.len()
	}
}

// finish 流结束时的兜底检测：短文本可能一直没过 repCheckEvery 节流阀（例如 10 次
// 复读共几百字节），不兜底就会漏报。幂等（已命中则空操作）。
func (d *repDiag) finish() {
	d.check(&d.reasoning, "reasoning")
	d.check(&d.content, "content")
}

// Hit 返回命中详情。ok=false 表示本次流未检出退化重复。
// 首次调用会做一次流结束兜底检测（finish），保证短流不漏报。
func (d *repDiag) Hit() (kind, unit string, count, reasonBytes, contentBytes int, ok bool) {
	d.finish()
	if !d.hit {
		return "", "", 0, 0, 0, false
	}
	return d.hitKind, d.hitUnit, d.hitCount, d.hitReason, d.hitContent, true
}

// ReasoningText / ContentText 返回已累积文本（有界，供落盘）。
func (d *repDiag) ReasoningText() string { return string(d.reasoning.data) }
func (d *repDiag) ContentText() string   { return string(d.content.data) }

// dumpRespDir 解析落盘开关。优先级：环境变量 WB2A_DUMP_RESP > 哨兵文件。
//   - 环境变量空/false → 再看哨兵文件 /app/data/dump_resp.on 是否存在（运行中可切）；
//   - 1/true/yes/on → 默认目录 /app/data；
//   - 其余值 → 当作显式目录。
//
// 哨兵文件的意义：env 只在进程启动时读取，而模型复读是偶发的——需要"现在就在抓"
// 时不必重启网关（重启会掐断在途请求）。touch 一个文件即可开抓。
func dumpRespDir() string {
	v := strings.TrimSpace(os.Getenv("WB2A_DUMP_RESP"))
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return dumpDefaultDir
	case "", "0", "false", "no", "off":
		// 落到哨兵文件判定
	default:
		return v
	}
	if _, err := os.Stat(dumpSentinelPath); err == nil {
		return dumpDefaultDir
	}
	return ""
}

// DumpRepetition 把请求体与已累积的思维链/正文落盘（dir 非空时）。写入失败只打
// WARN，绝不影响请求本身（调试功能不拖垮主链路）。
func DumpRepetition(dir string, reqBody []byte, reasoning, content string) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("WARN: [server] dump resp mkdir %s: %v", dir, err)
		return
	}
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			log.Printf("WARN: [server] dump %s: %v", name, err)
		}
	}
	if len(reqBody) > 0 {
		write("last_request.json", reqBody)
	}
	write("last_reasoning.txt", []byte(reasoning))
	write("last_content.txt", []byte(content))
}

// diagFromChatResponse 对非流式聚合响应做一次重复检测：取出首条 choice 的
// reasoning_content / content 喂入累积器。resp 形态不符时返回空累积器（Hit=false）。
func diagFromChatResponse(resp map[string]any) *repDiag {
	d := &repDiag{}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return d
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return d
	}
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		return d
	}
	d.feedReasoning(strAny(msg["reasoning_content"]))
	d.feedContent(strAny(msg["content"]))
	return d
}

// logRepetition 打一行退化重复 WARN，附复读单元样例（截断）与规模。
func logRepetition(acctLabel, model, effort string, d *repDiag) {
	kind, unit, count, rb, cb, ok := d.Hit()
	if !ok {
		return
	}
	log.Printf("WARN: [server] degenerate repetition acct=%s model=%s effort=%s kind=%s unit=%q repeats=%d reasoning_bytes=%d content_bytes=%d (model stuck in a loop)",
		acctLabel, model, effort, kind, clipForLog(unit, 200), count, rb, cb)
}

// clipForLog 把单元样例截到 max 字节并加省略号（日志行不失控）。
func clipForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// 按 rune 边界截断，避免截出半个多字节字符。
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// utf8Start 判断 b 是否是一个 UTF-8 字符的起始字节（续字节形如 10xxxxxx）。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
