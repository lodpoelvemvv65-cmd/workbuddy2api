package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// chainIDs 把候选链渲染成 "realm|id" 便于断言顺序。
func chainIDs(cands []modelCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Realm+"|"+c.ID)
	}
	return out
}

func assertChain(t *testing.T, got []modelCandidate, want []string) {
	t.Helper()
	g := chainIDs(got)
	if len(g) != len(want) {
		t.Fatalf("chain = %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("chain = %v, want %v", g, want)
		}
	}
}

// TestParseContextMarker [1m]/[200k]/[1000000] 的档位换算与净名提取。
func TestParseContextMarker(t *testing.T) {
	cases := []struct {
		in      string
		wantID  string
		wantCtx int64
	}{
		{"deepseek-flash", "deepseek-flash", 0},
		{"deepseek-flash[1m]", "deepseek-flash", 1000000},
		{"deepseek-flash[1M]", "deepseek-flash", 1000000},
		{"deepseek-flash[200k]", "deepseek-flash", 200000},
		{"deepseek-flash[1000000]", "deepseek-flash", 1000000},
		{"deepseek-flash[0m]", "deepseek-flash", 0}, // 非法档位：退回无需求，不臆造
		{"[1m]", "[1m]", 0},                         // 无模型名的畸形输入：原样
	}
	for _, c := range cases {
		id, ctx := parseContextMarker(c.in)
		if id != c.wantID || ctx != c.wantCtx {
			t.Errorf("parseContextMarker(%q) = (%q,%d), want (%q,%d)", c.in, id, ctx, c.wantID, c.wantCtx)
		}
	}
}

// TestCanonicalModelName 同名家族归一化：剥 realm 前缀 / 上下文标记 / -sg 区域后缀，
// 大小写折叠。
func TestCanonicalModelName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"global:deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"cn:deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"deepseek-v4.1-flash-sg", "deepseek-v4.1-flash"},
		{"global:deepseek-v4.1-flash-sg", "deepseek-v4.1-flash"},
		{"deepseek-v4.1-flash[1M]", "deepseek-v4.1-flash"},
		{"global:DeepSeek-V4.1-Flash[1M]", "deepseek-v4.1-flash"},
		{"glm-5.3-flash", "glm-5.3-flash"}, // 不带后缀的名字不受影响
	}
	for _, c := range cases {
		if got := canonicalModelName(c.in); got != c.want {
			t.Errorf("canonicalModelName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseCredits 倍率原文解析：区分「明确免费（0）」与「未知（无倍率信息）」。
func TestParseCredits(t *testing.T) {
	cases := []struct {
		in    string
		want  float64
		known bool
	}{
		{"x0.00", 0, true},
		{"x0.00 credits", 0, true},
		{"x0.03", 0.03, true},
		{"x3.31 credits", 3.31, true},
		{"0.05", 0.05, true},
		{"", 0, false},
		{"credits", 0, false},
	}
	for _, c := range cases {
		got, known := parseCredits(c.in)
		if got != c.want || known != c.known {
			t.Errorf("parseCredits(%q) = (%v,%v), want (%v,%v)", c.in, got, known, c.want, c.known)
		}
	}
}

// newChainHandler 只读快照依赖的 Handler：池里给一个 CN 号，global 关掉
// （global 目录快照在这个测试里无法种，聚焦同域候选链行为）。
func newChainHandler(t *testing.T) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999}),
		Upstream: &upstream.Client{},
	})
}

// TestModelChainFreeFirst 同名家族按「免费 → 积分」排序，且客户端写不带后缀的名字
// 也能拿到含 -sg 变体的完整家族（"不区别 global/sg，只有模型名称"）。
func TestModelChainFreeFirst(t *testing.T) {
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash-sg", Credits: "x0.03", ContextWindow: 1000000},
		{ID: "deepseek-v4.1-flash", Credits: "x0.00", ContextWindow: 1000000},
		{ID: "glm-5.3-flash", Credits: "x0.06", ContextWindow: 1000000},
	})
	h := newChainHandler(t)

	// 客户端写客户端名 → 免费档在前，积分档在后。
	assertChain(t, h.modelChain("deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"), []string{
		"cn|deepseek-v4.1-flash",
		"cn|deepseek-v4.1-flash-sg",
	})
	// 客户端显式写 -sg 变体 → 归一到同一家族，仍是免费档优先（"不区别 sg"）。
	assertChain(t, h.modelChain("deepseek-v4.1-flash-sg", "cn", "deepseek-v4.1-flash-sg"), []string{
		"cn|deepseek-v4.1-flash",
		"cn|deepseek-v4.1-flash-sg",
	})
	// 无关模型不串族。
	assertChain(t, h.modelChain("glm-5.3-flash", "cn", "glm-5.3-flash"), []string{"cn|glm-5.3-flash"})
}

// TestModelChain1MMarker [1M] 标记只做档位过滤：有满足的候选就只留它们，
// 一个都没有则退回全部（上游没有 1M 就用原来的，不退化成报错）。
func TestModelChain1MMarker(t *testing.T) {
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", Credits: "x0.03", ContextWindow: 200000},
	})
	h := newChainHandler(t)

	assertChain(t, h.modelChain("deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"), []string{
		"cn|deepseek-v4.1-flash",
	})
	// 目录里没有 1M 档 → 退回原模型（want 已剥标记），不是空链也不是报错。
	assertChain(t, h.modelChain("deepseek-v4.1-flash[1M]", "cn", "deepseek-v4.1-flash[1M]"), []string{
		"cn|deepseek-v4.1-flash",
	})
	// 目录里两个档位时，[1M] 只留满足的那个。
	seedModelsCache([]upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", Credits: "x0.03", ContextWindow: 200000},
		{ID: "deepseek-v4.1-flash-sg", Credits: "x0.00", ContextWindow: 1000000},
	})
	assertChain(t, h.modelChain("deepseek-v4.1-flash[1M]", "cn", "deepseek-v4.1-flash[1M]"), []string{
		"cn|deepseek-v4.1-flash-sg",
	})
}

// TestModelChainUnknownPassthrough 目录未收录（含缓存冷）→ 单候选透传，
// 模型名只被剥掉 [1M] 标记，其余原样——行为与引入候选链之前一致。
func TestModelChainUnknownPassthrough(t *testing.T) {
	defer resetModelsCache()
	resetModelsCache()
	h := newChainHandler(t)

	assertChain(t, h.modelChain("someone-else-model", "cn", "someone-else-model"), []string{
		"cn|someone-else-model",
	})
	assertChain(t, h.modelChain("someone-else-model[1M]", "cn", "someone-else-model[1M]"), []string{
		"cn|someone-else-model",
	})
}

// TestChatFallsBackFromFreeToPaidModel 端到端：免费档被上游 6004 限流（全池无可用号）
// 时，同一请求自动沿候选链换到积分档，客户端拿到 200 而不是 503。
func TestChatFallsBackFromFreeToPaidModel(t *testing.T) {
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", Credits: "x0.00", ContextWindow: 1000000},
		{ID: "deepseek-v4.1-flash-sg", Credits: "x0.03", ContextWindow: 1000000},
	})

	var models []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var peek struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(raw, &peek)
			models = append(models, peek.Model)
			if peek.Model == "deepseek-v4.1-flash" {
				return &http.Response{
					StatusCode: 429,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"code":6004,"msg":"usage exceeds frequency limit, your usage will reset at 2026-09-23 16:40:54 UTC+8"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(models) != 2 || models[0] != "deepseek-v4.1-flash" || models[1] != "deepseek-v4.1-flash-sg" {
		t.Fatalf("outbound models = %v, want [免费档 积分档]", models)
	}
}

// TestModelChainColdSnapshotTriggersBackgroundWarm 快照冷时：本次请求不阻塞、
// 仍按单候选透传走，后台补一次目录预热（下一个请求即可拿到完整候选链）。
func TestModelChainColdSnapshotTriggersBackgroundWarm(t *testing.T) {
	defer resetModelsCache()
	resetModelsCache()

	var calls atomic.Int32
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 200, `{"data":[]}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ColdCatalogWarm: true})

	// 冷快照：本次调用必须立刻返回单候选透传，不等预热。
	assertChain(t, h.modelChain("deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"), []string{
		"cn|deepseek-v4.1-flash",
	})

	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("冷快照未触发后台预热：目录端点一次都没被请求")
	}
}

// TestModelChainColdSnapshotWarmDisabled 开关关闭时（测试默认形态）不得起后台
// goroutine、不得打上游目录端点——这是测试确定性的前提。
func TestModelChainColdSnapshotWarmDisabled(t *testing.T) {
	defer resetModelsCache()
	resetModelsCache()

	var calls atomic.Int32
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 200, `{"data":[]}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // ColdCatalogWarm 缺省 false

	assertChain(t, h.modelChain("deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"), []string{
		"cn|deepseek-v4.1-flash",
	})
	time.Sleep(150 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("开关关闭却打了上游：%d 次", n)
	}
}

// TestRewriteAliasKeepingAffixes 换别名只换主体：realm 前缀与 [1M] 标记要保留，
// 目标自带前缀时以目标为准。
func TestRewriteAliasKeepingAffixes(t *testing.T) {
	cases := []struct{ model, target, want string }{
		{"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"deepseek-flash[1M]", "deepseek-v4.1-flash", "deepseek-v4.1-flash[1M]"},
		{"global:deepseek-flash", "deepseek-v4.1-flash", "global:deepseek-v4.1-flash"},
		{"global:deepseek-flash[1M]", "deepseek-v4.1-flash", "global:deepseek-v4.1-flash[1M]"},
		{"deepseek-flash", "global:gpt-5.6-luna", "global:gpt-5.6-luna"},
		{"cn:deepseek-flash", "global:gpt-5.6-luna", "global:gpt-5.6-luna"},
		{"deepseek-flash[200k]", "global:gpt-5.6-luna", "global:gpt-5.6-luna[200k]"},
	}
	for _, c := range cases {
		if got := rewriteAliasKeepingAffixes(c.model, c.target); got != c.want {
			t.Errorf("rewriteAliasKeepingAffixes(%q,%q) = %q, want %q", c.model, c.target, got, c.want)
		}
	}
}

// TestAliasModel 别名查表：键按同名家族归一化（大小写 / 前缀 / 标记 / -sg 都不影响命中），
// 未命中原样返回。
func TestAliasModel(t *testing.T) {
	// 键故意写成大小写混合 + 带标记的形态：NewHandler 应把它归一到 canonical 键。
	h := NewHandler(Config{
		Pool:         testPoolWith(),
		Upstream:     &upstream.Client{},
		ModelAliases: map[string]string{"DeepSeek-Flash[1M]": "deepseek-v4.1-flash"},
	})

	cases := []struct {
		in      string
		want    string
		wantHit bool
	}{
		{"deepseek-flash", "deepseek-v4.1-flash", true},
		{"DEEPSEEK-FLASH", "deepseek-v4.1-flash", true},
		{"deepseek-flash[1M]", "deepseek-v4.1-flash[1M]", true},
		{"deepseek-flash-sg", "deepseek-v4.1-flash", true}, // -sg 归一到同族，命中同一条别名
		{"global:deepseek-flash", "global:deepseek-v4.1-flash", true},
		{"glm-5.3-flash", "glm-5.3-flash", false}, // 未配置 → 原样
		{"", "", false},
	}
	for _, c := range cases {
		got, hit := h.aliasModel(c.in)
		if got != c.want || hit != c.wantHit {
			t.Errorf("aliasModel(%q) = (%q,%v), want (%q,%v)", c.in, got, hit, c.want, c.wantHit)
		}
	}

	// 空表 = 不启用：任何名字都原样返回。
	plain := NewHandler(Config{Pool: testPoolWith(), Upstream: &upstream.Client{}})
	if got, hit := plain.aliasModel("deepseek-flash"); got != "deepseek-flash" || hit {
		t.Errorf("空别名表不应改写: (%q,%v)", got, hit)
	}
}

// TestChatAppliesModelAlias 端到端：客户端写短名，出站 body 的 model 换成真实模型名。
func TestChatAppliesModelAlias(t *testing.T) {
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", Credits: "x0.00", ContextWindow: 1000000},
	})

	var sent []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var peek struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(raw, &peek)
			sent = append(sent, peek.Model)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		ModelAliases: map[string]string{"deepseek-flash": "deepseek-v4.1-flash"},
	})

	// 三态都要覆盖：**裸短名**是别名替换后 bareModel==peek.Model 的形态——body 改写
	// 只挂在 bareModel != peek.Model 上（那是给 realm 前缀/上下文标记用的），别名若
	// 不自己改写 body，短名就会直传上游（实测 11102 model not available × 全号）。
	for _, c := range []struct{ in, want string }{
		{"deepseek-flash", "deepseek-v4.1-flash"},
		{"deepseek-flash[1M]", "deepseek-v4.1-flash"},
		{"global:deepseek-flash", "deepseek-v4.1-flash"},
	} {
		sent = nil
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+c.in+`","messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != 200 {
			t.Fatalf("%s: code=%d body=%s", c.in, rec.Code, rec.Body)
		}
		if len(sent) != 1 || sent[0] != c.want {
			t.Fatalf("%s: outbound model = %v, want [%s]", c.in, sent, c.want)
		}
	}
}
