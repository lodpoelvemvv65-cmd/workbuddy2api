package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withMetricsPath 把全局聚合表切到临时持久化路径，并在用例结束后复原纯内存态。
func withMetricsPath(t *testing.T, path string) {
	t.Helper()
	m := globalMetrics
	m.mu.Lock()
	m.path = path
	m.dirty = false
	m.persistFails = 0
	m.mu.Unlock()
	t.Cleanup(func() {
		m.mu.Lock()
		m.path = ""
		m.dirty = false
		m.persistFails = 0
		m.mu.Unlock()
	})
}

// TestMetricsPersistRoundTrip 落盘后清空内存再加载，累计量/窗口还原。
func TestMetricsPersistRoundTrip(t *testing.T) {
	resetMetricsForTest(t)
	path := filepath.Join(t.TempDir(), "metrics.json")
	withMetricsPath(t, path)

	recordChatMetric(&chatStat{
		model: "global:deepseek-v4.1-flash", mode: "stream", status: 200,
		ttfb: 1200 * time.Millisecond, toks: 50, hasUsage: true, prompt: 10,
		cacheHit: 8, cacheMiss: 2, credit: 0.03, hasCredit: true,
	}, 4*time.Second)
	FlushMetrics()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}

	// 模拟进程重启：清空内存（不置脏，避免覆盖文件），再从文件恢复。
	m := globalMetrics
	m.mu.Lock()
	m.byModel = map[string]*modelMetrics{}
	m.since = time.Time{}
	m.loadLocked()
	m.mu.Unlock()

	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 1 || snap.Total.Success != 1 {
		t.Fatalf("重启后 total req/succ = %d/%d want 1/1", snap.Total.Requests, snap.Total.Success)
	}
	if snap.Total.PromptTokens != 10 || snap.Total.CompletionTokens != 50 {
		t.Errorf("重启后 tokens = %d/%d want 10/50", snap.Total.PromptTokens, snap.Total.CompletionTokens)
	}
	if snap.Total.CacheHitTokens != 8 || snap.Total.CacheMissTokens != 2 {
		t.Errorf("重启后 cache = %d/%d want 8/2", snap.Total.CacheHitTokens, snap.Total.CacheMissTokens)
	}
	if snap.Total.Credit != 0.03 {
		t.Errorf("重启后 credit = %v want 0.03", snap.Total.Credit)
	}
	if snap.Total.Streaming != 1 {
		t.Errorf("重启后 streaming = %d want 1", snap.Total.Streaming)
	}
	// 统计窗口（since）跨重启延续，不再随进程启动时间重置。
	if snap.Since.IsZero() || !snap.Since.Before(time.Now()) {
		t.Errorf("since 未从文件恢复: %v", snap.Since)
	}
}

// TestStartMetricsPersistenceLoadsExisting 启动接线时加载已有文件。
func TestStartMetricsPersistenceLoadsExisting(t *testing.T) {
	resetMetricsForTest(t)
	path := filepath.Join(t.TempDir(), "metrics.json")
	raw := []byte(`{"since":"2020-01-01T00:00:00Z","models":{"m1":{"requests":7,"success":7,"prompt_tokens":3}}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	stop := StartMetricsPersistence(path)
	t.Cleanup(stop)

	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 7 || snap.Total.Success != 7 || snap.Total.PromptTokens != 3 {
		t.Errorf("加载后 = req %d succ %d prompt %d want 7/7/3",
			snap.Total.Requests, snap.Total.Success, snap.Total.PromptTokens)
	}
	want := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if !snap.Since.Equal(want) {
		t.Errorf("since = %v want %v", snap.Since, want)
	}
}

// TestStartMetricsPersistenceEmptyPathNoop 空路径 = 纯内存旧行为，stop 可安全调用。
func TestStartMetricsPersistenceEmptyPathNoop(t *testing.T) {
	resetMetricsForTest(t)
	stop := StartMetricsPersistence("")
	stop()
	stop() // 二次调用也不得 panic（空操作闭包）
}

// TestStartMetricsPersistenceStopIdempotent 非空路径的 stop 幂等（公共 API 防御）。
func TestStartMetricsPersistenceStopIdempotent(t *testing.T) {
	resetMetricsForTest(t)
	path := filepath.Join(t.TempDir(), "metrics.json")
	stop := StartMetricsPersistence(path)
	stop()
	stop() // 二次调用不得因重复 close(channel) panic
}

// TestMetricsResetPersistsEmpty 重置结果必须立即落盘，避免重启后旧数据复活。
func TestMetricsResetPersistsEmpty(t *testing.T) {
	resetMetricsForTest(t)
	path := filepath.Join(t.TempDir(), "metrics.json")
	withMetricsPath(t, path)

	recordChatMetric(&chatStat{model: "m1", mode: "sync", status: 200, toks: 5, hasUsage: true}, time.Second)
	FlushMetrics()
	ResetMetrics()
	FlushMetrics()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pm persistedMetrics
	if err := json.Unmarshal(raw, &pm); err != nil {
		t.Fatal(err)
	}
	if len(pm.Models) != 0 {
		t.Errorf("重置后落盘 models=%d want 0", len(pm.Models))
	}
}

// TestMetricsLoadCorruptIgnored 非法 JSON 静默零状态，不 panic、不污染。
func TestMetricsLoadCorruptIgnored(t *testing.T) {
	resetMetricsForTest(t)
	path := filepath.Join(t.TempDir(), "metrics.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	withMetricsPath(t, path)

	m := globalMetrics
	m.mu.Lock()
	m.loadLocked()
	m.mu.Unlock()

	if len(MetricsSnapshotOf().Models) != 0 {
		t.Errorf("损坏文件不应产生任何模型条目")
	}
}

// TestMetricsLoadDropsNegativeEntries 负计数（结构破损）条目剔除，合法条目保留。
func TestMetricsLoadDropsNegativeEntries(t *testing.T) {
	resetMetricsForTest(t)
	path := filepath.Join(t.TempDir(), "metrics.json")
	raw := []byte(`{"models":{"good":{"requests":1,"success":1},"bad":{"requests":-5},"bad2":{"cache_hit_tokens":-1}}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	withMetricsPath(t, path)

	m := globalMetrics
	m.mu.Lock()
	m.loadLocked()
	m.mu.Unlock()

	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 || snap.Models[0].Model != "good" {
		t.Fatalf("models = %+v want 仅 good", snap.Models)
	}
}

// TestMetricsNotPersistedWithoutPath 未接线（path 空）时不落盘、dirty 不置位。
func TestMetricsNotPersistedWithoutPath(t *testing.T) {
	resetMetricsForTest(t)
	recordChatMetric(&chatStat{model: "m1", mode: "sync", status: 200, toks: 1, hasUsage: true}, time.Second)

	m := globalMetrics
	m.mu.Lock()
	dirty := m.dirty
	m.mu.Unlock()
	if dirty {
		t.Errorf("path 为空时不应置 dirty")
	}
	FlushMetrics() // 不应 panic / 写文件
}
