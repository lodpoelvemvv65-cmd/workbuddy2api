package server

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func TestDetectRepeatBlock(t *testing.T) {
	unit := "Let me run. Let me do it. Let me go. OK. Running. "
	text := strings.Repeat(unit, 20)
	gotUnit, gotCnt, ok := detectRepeat([]byte(text))
	if !ok {
		t.Fatalf("detectRepeat 未命中明显循环:\n%s", text)
	}
	if gotUnit != unit {
		t.Errorf("unit=%q want %q", gotUnit, unit)
	}
	if gotCnt < 4 {
		t.Errorf("count=%d want >=4", gotCnt)
	}
}

func TestDetectRepeatNormalText(t *testing.T) {
	text := "这是一段正常的中文回答，没有明显的重复循环。" +
		"接下来解释一下为什么会出现这个问题，以及如何修复它。"
	if _, _, ok := detectRepeat([]byte(text)); ok {
		t.Error("正常文本被误判为退化重复")
	}
	// 太短不检测。
	if _, _, ok := detectRepeat([]byte("abcabc")); ok {
		t.Error("短文本不应命中")
	}
	// 纯空白重复不算退化信号。
	if _, _, ok := detectRepeat([]byte(strings.Repeat("    ", 100))); ok {
		t.Error("纯空白重复不应命中")
	}
}

func TestRepDiagFeedChunked(t *testing.T) {
	unit := "Let me run. Let me do it. Let me go. OK. Running. "
	var d repDiag
	// 逐小块喂入（模拟 SSE delta）。
	full := strings.Repeat(unit, 30)
	for i := 0; i < len(full); i += 7 {
		end := i + 7
		if end > len(full) {
			end = len(full)
		}
		d.feedReasoning(full[i:end])
	}
	kind, gotUnit, cnt, rb, cb, ok := d.Hit()
	if !ok {
		t.Fatal("chunked feed 未命中循环")
	}
	if kind != "reasoning" {
		t.Errorf("kind=%q want reasoning", kind)
	}
	// 检测点可能落在复读单元中间，返回的 unit 是真实循环单元的一个「旋转」
	// （长度相同、整体仍是原文子串）——对诊断已足够，断言到这一步即可。
	if len(gotUnit) != len(unit) || !strings.Contains(full, gotUnit) {
		t.Errorf("unit=%q 不是 %q 的旋转", gotUnit, unit)
	}
	if cnt < 4 || rb == 0 || cb != 0 {
		t.Errorf("count=%d reasonBytes=%d contentBytes=%d", cnt, rb, cb)
	}
}

func TestDiagFromChatResponse(t *testing.T) {
	unit := "I will check. Let me verify. OK. Done. "
	resp := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"reasoning_content": strings.Repeat(unit, 10),
					"content":           "final answer",
				},
			},
		},
	}
	d := diagFromChatResponse(resp)
	if _, _, _, _, _, ok := d.Hit(); !ok {
		t.Fatal("非流式聚合响应未命中循环")
	}
	// 形态不符：不 panic、不命中。
	if _, _, _, _, _, ok := diagFromChatResponse(map[string]any{}).Hit(); ok {
		t.Error("空响应不应命中")
	}
}

func TestChatStatsReaderFeedsRepDiag(t *testing.T) {
	unit := "Let me run. Let me do it. Let me go. OK. Running. "
	var b strings.Builder
	// 拆成多个 SSE delta 帧。
	for i := 0; i < 30; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"reasoning_content":`)
		b.WriteString(jsonQuote(unit))
		b.WriteString("}}]}\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	r := newChatStatsReaderSince(strings.NewReader(b.String()), time.Now())
	for {
		if _, err := r.Read(make([]byte, 64)); err != nil {
			break
		}
	}
	if _, _, _, _, _, ok := r.diag.Hit(); !ok {
		t.Fatal("chatStatsReader 未把 delta 喂给 repDiag")
	}
}

func TestLogRepetitionFormat(t *testing.T) {
	var d repDiag
	unit := "Let me run. Let me do it. Let me go. OK. Running. "
	d.feedReasoning(strings.Repeat(unit, 50))

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	logRepetition("acct(nick)", "deepseek-v4.1-flash", "high", &d)

	out := buf.String()
	for _, want := range []string{
		"degenerate repetition",
		"acct=acct(nick)",
		"model=deepseek-v4.1-flash",
		"effort=high",
		"kind=reasoning",
		"repeats=",
		"reasoning_bytes=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WARN 缺字段 %q\n%s", want, out)
		}
	}
	t.Logf("实际日志行: %s", strings.TrimSpace(out))
}

func TestDetectRepeatSeparatorNotFlagged(t *testing.T) {
	// 分隔线（单字符重复）不应被判为退化复读。
	seps := []string{
		strings.Repeat("─", 30),
		strings.Repeat("=", 32),
		strings.Repeat(".", 40),
		strings.Repeat("- ", 20),
		strings.Repeat("\u3000", 20),
	}
	for _, s := range seps {
		if u, c, ok := detectRepeat([]byte(s)); ok {
			t.Errorf("分隔线被误报为复读: unit=%q repeats=%d input=%q", u, c, s)
		}
	}
}

func TestUniformUnit(t *testing.T) {
	if !uniformUnit([]byte("────────")) {
		t.Error("全横线应为 uniform")
	}
	if !uniformUnit([]byte("====")) {
		t.Error("全等号应为 uniform")
	}
	if uniformUnit([]byte("Hmm. Hmm.")) {
		t.Error("含多种字符不应为 uniform")
	}
	if uniformUnit([]byte("\u3000\u3000")) {
		t.Error("全空白应视为 uniform（排除）")
	}
}

// jsonQuote 极简 JSON 字符串转义（测试用，内容仅 ASCII 安全字符）。
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
