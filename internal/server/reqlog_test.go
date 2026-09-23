package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mkStat 构造一个用于流水缓冲测试的 chatStat（字段取值刻意覆盖各分支）。
func mkStat(model string, status, toks int) *chatStat {
	return &chatStat{
		start:     time.Now().Add(-2 * time.Second),
		model:     model,
		mode:      "stream",
		uid:       "00e26541abcdef",
		nick:      "sample",
		ttfb:      400 * time.Millisecond,
		toks:      toks,
		status:    status,
		hasUsage:  true,
		prompt:    100,
		cacheHit:  80,
		cacheMiss: 20,
		credit:    0.5,
		hasCredit: true,
	}
}

func TestRequestLogRingOrderAndLimit(t *testing.T) {
	resetRequestLog()
	t.Cleanup(resetRequestLog)

	for i := 1; i <= 5; i++ {
		appendRequestLog(mkStat("m", 200, i), time.Second, int64(i))
	}

	got := RecentRequestLogs(0)
	if len(got) != 5 {
		t.Fatalf("len=%d want 5", len(got))
	}
	// 最新在前：seq 5,4,3,2,1。
	for i, e := range got {
		if want := int64(5 - i); e.Seq != want {
			t.Errorf("entry[%d].Seq=%d want %d", i, e.Seq, want)
		}
	}

	if lim := RecentRequestLogs(2); len(lim) != 2 || lim[0].Seq != 5 || lim[1].Seq != 4 {
		t.Errorf("limit=2 got %+v", lim)
	}

	// 字段映射：token / 缓存 / 扣费 / 账号。
	e := got[0]
	if e.CompletionTokens != 5 || e.PromptTokens != 100 {
		t.Errorf("tokens=%d/%d", e.PromptTokens, e.CompletionTokens)
	}
	if e.CacheHitTokens != 80 || e.CacheMissTokens != 20 {
		t.Errorf("cache=%d/%d", e.CacheHitTokens, e.CacheMissTokens)
	}
	if !e.HasCredit || e.Credit != 0.5 {
		t.Errorf("credit=%v has=%v", e.Credit, e.HasCredit)
	}
	if e.UID != "00e26541abcdef" || e.Nickname != "sample" {
		t.Errorf("account=%q/%q", e.UID, e.Nickname)
	}
	if e.TTFBMS != 400 {
		t.Errorf("ttfb=%d", e.TTFBMS)
	}
}

func TestRequestLogWrapsAtCapacity(t *testing.T) {
	resetRequestLog()
	t.Cleanup(resetRequestLog)

	for i := 1; i <= requestLogCapacity+3; i++ {
		appendRequestLog(mkStat("m", 200, 1), time.Second, int64(i))
	}
	capacity, count, total := RequestLogStats()
	if capacity != requestLogCapacity || count != requestLogCapacity {
		t.Fatalf("capacity=%d count=%d want %d/%d", capacity, count, requestLogCapacity, requestLogCapacity)
	}
	if total != int64(requestLogCapacity+3) {
		t.Errorf("total=%d want %d", total, requestLogCapacity+3)
	}
	got := RecentRequestLogs(0)
	if len(got) != requestLogCapacity {
		t.Fatalf("len=%d want %d", len(got), requestLogCapacity)
	}
	// 最旧一条应是第 4 条（前 3 条被覆盖），最新是第 capacity+3 条。
	if got[0].Seq != int64(requestLogCapacity+3) {
		t.Errorf("newest seq=%d", got[0].Seq)
	}
	if last := got[len(got)-1]; last.Seq != 4 {
		t.Errorf("oldest surviving seq=%d want 4", last.Seq)
	}
}

func TestAppendRequestLogNoUsageSentinel(t *testing.T) {
	resetRequestLog()
	t.Cleanup(resetRequestLog)

	st := mkStat("m", http.StatusServiceUnavailable, -1)
	st.hasUsage = false
	st.hasCredit = false
	appendRequestLog(st, time.Second, 1)

	e := RecentRequestLogs(1)[0]
	if e.CompletionTokens != -1 {
		t.Errorf("completion_tokens=%d want -1 sentinel", e.CompletionTokens)
	}
	if e.HasUsage || e.HasCredit {
		t.Errorf("has_usage/has_credit must be false for missing usage")
	}
	if e.TokensPerSec != 0 {
		t.Errorf("tokens_per_sec=%v want 0 (no observation)", e.TokensPerSec)
	}
}

func TestParseLogLimit(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", defaultLogLimit},
		{"25", 25},
		{"0", defaultLogLimit},
		{"-3", defaultLogLimit},
		{"abc", defaultLogLimit},
		{"999999", maxLogLimit},
	}
	for _, c := range cases {
		if got := parseLogLimit(c.in); got != c.want {
			t.Errorf("parseLogLimit(%q)=%d want %d", c.in, got, c.want)
		}
	}
}

func TestLogsEndpoint(t *testing.T) {
	resetRequestLog()
	t.Cleanup(resetRequestLog)
	appendRequestLog(mkStat("deepseek-v4.1-flash", 200, 42), 1500*time.Millisecond, 7)

	h := NewHandler(Config{Pool: testPoolWith(), APIKey: "k"})

	// 无 key → 401（与其它 /v1 端点同鉴权）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/logs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth code=%d want 401", rec.Code)
	}

	// 带 key → 200，entries 含刚写入的一条。
	req := httptest.NewRequest("GET", "/v1/logs?limit=10", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var snap RequestLogSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !snap.Enabled || snap.Capacity != requestLogCapacity || len(snap.Entries) != 1 {
		t.Fatalf("snapshot=%+v", snap)
	}
	e := snap.Entries[0]
	if e.Seq != 7 || e.Model != "deepseek-v4.1-flash" || e.CompletionTokens != 42 {
		t.Errorf("entry=%+v", e)
	}
}
