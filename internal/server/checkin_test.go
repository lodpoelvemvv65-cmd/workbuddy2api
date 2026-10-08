package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// checkinPost 发一次 POST /v1/checkin（不带鉴权，除非调用方自己加头）。
func checkinPost(t *testing.T, h *Handler, authz string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/checkin", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCheckinNotWired 未接线 CheckinFn 时必须回 501，而不是 200 空报告或 panic。
func TestCheckinNotWired(t *testing.T) {
	h := NewHandler(Config{})
	rec := checkinPost(t, h, "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501, body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "not_wired")
}

// TestCheckinOK 正常路径：200 + 报告原样透出（含每号 realm / status / 余额）。
func TestCheckinOK(t *testing.T) {
	credits := int64(2100)
	h := NewHandler(Config{CheckinFn: func() (CheckinReport, bool, error) {
		return CheckinReport{
			Enabled: true, Hours: []int{9, 21}, Total: 2,
			OK: 1, Already: 1,
			Results: []CheckinResult{
				{UID: "u-cn", Nickname: "online", Realm: "cn", Status: "ok", Credits: &credits},
				{UID: "u-gl", Nickname: "clay", Realm: "global", Status: "already"},
			},
		}, false, nil
	}})
	rec := checkinPost(t, h, "")
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200, body=%s", rec.Code, rec.Body)
	}
	var got CheckinReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body)
	}
	if !got.Enabled || len(got.Hours) != 2 || got.Hours[0] != 9 || got.Hours[1] != 21 {
		t.Errorf("排程字段丢失: %+v", got)
	}
	if got.Total != 2 || got.OK != 1 || got.Already != 1 {
		t.Errorf("汇总不符: %+v", got)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results 数量=%d want 2", len(got.Results))
	}
	if got.Results[0].Realm != "cn" || got.Results[0].Status != "ok" {
		t.Errorf("results[0] 不符: %+v", got.Results[0])
	}
	if got.Results[0].Credits == nil || *got.Results[0].Credits != 2100 {
		t.Errorf("results[0].Credits 丢失: %+v", got.Results[0])
	}
	// global 号无签到但有余额查询 → 必须有 skipped/already 之类状态且不被吞掉。
	if got.Results[1].Realm != "global" {
		t.Errorf("results[1] 不符: %+v", got.Results[1])
	}
}

// TestCheckinResultsNeverNull results 为 nil 时必须序列化成 []（前端直接 .map 会炸）。
func TestCheckinResultsNeverNull(t *testing.T) {
	h := NewHandler(Config{CheckinFn: func() (CheckinReport, bool, error) {
		return CheckinReport{}, false, nil
	}})
	rec := checkinPost(t, h, "")
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"results":[]`) {
		t.Errorf("results 必须是 [] 而不是 null, body=%s", rec.Body)
	}
}

// TestCheckinBusy 撞车（scheduler.ErrBusy）→ 429 busy，且**不占用冷却窗口**：
// busy 只是"别人正在跑"，不该让用户随后白等 30s。
func TestCheckinBusy(t *testing.T) {
	calls := 0
	h := NewHandler(Config{CheckinFn: func() (CheckinReport, bool, error) {
		calls++
		return CheckinReport{}, true, nil
	}})
	rec := checkinPost(t, h, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429, body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "busy")

	rec2 := checkinPost(t, h, "")
	if calls != 2 {
		t.Fatalf("busy 不该盖章冷却：CheckinFn 调用次数=%d want 2", calls)
	}
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("第二次仍应是 busy(429)，实际 %d", rec2.Code)
	}
}

// TestCheckinCooldown 成功一次后立刻再调 → 429 cooldown + Retry-After，
// 且 CheckinFn 不得被二次调用（冷却的意义就是别重复打上游）。
func TestCheckinCooldown(t *testing.T) {
	calls := 0
	h := NewHandler(Config{CheckinFn: func() (CheckinReport, bool, error) {
		calls++
		return CheckinReport{Total: 1}, false, nil
	}})
	if rec := checkinPost(t, h, ""); rec.Code != 200 {
		t.Fatalf("首次 code=%d want 200", rec.Code)
	}
	rec2 := checkinPost(t, h, "")
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("二次 code=%d want 429 (cooldown), body=%s", rec2.Code, rec2.Body)
	}
	assertJSONErrorCode(t, rec2.Body.String(), "cooldown")
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("cooldown 响应必须带 Retry-After 头")
	}
	if calls != 1 {
		t.Errorf("冷却期内不得再调 CheckinFn：calls=%d want 1", calls)
	}
}

// TestCheckinError 底层报错 → 500 checkin_failed，且不盖章（错误不该占用冷却窗口）。
func TestCheckinError(t *testing.T) {
	calls := 0
	h := NewHandler(Config{CheckinFn: func() (CheckinReport, bool, error) {
		calls++
		return CheckinReport{}, false, errors.New("pool exploded")
	}})
	rec := checkinPost(t, h, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d want 500, body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "checkin_failed")
	// 紧接着再调仍应真的进 CheckinFn（没被冷却挡住）。
	checkinPost(t, h, "")
	if calls != 2 {
		t.Errorf("报错不该盖章冷却：calls=%d want 2", calls)
	}
}

// TestCheckinRequiresAuth 配了 api_key 后未鉴权必须 401，且不得触达 CheckinFn
// （与 /v1/* 其余端点同源鉴权）。
func TestCheckinRequiresAuth(t *testing.T) {
	called := false
	h := NewHandler(Config{APIKey: "sk-test", CheckinFn: func() (CheckinReport, bool, error) {
		called = true
		return CheckinReport{}, false, nil
	}})
	if rec := checkinPost(t, h, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权 code=%d want 401", rec.Code)
	}
	if called {
		t.Fatal("未鉴权请求不得触达 CheckinFn")
	}
	if rec := checkinPost(t, h, "Bearer sk-test"); rec.Code != 200 {
		t.Fatalf("带密钥 code=%d want 200, body=%s", rec.Code, rec.Body)
	}
	if !called {
		t.Error("带正确密钥时应触达 CheckinFn")
	}
}

// TestCheckinWrongMethod GET 未注册 → 405（mux 按方法注册，GET 不该落到 POST 处理器）。
func TestCheckinWrongMethod(t *testing.T) {
	h := NewHandler(Config{CheckinFn: func() (CheckinReport, bool, error) {
		return CheckinReport{}, false, nil
	}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/checkin", nil))
	if rec.Code == 200 {
		t.Fatalf("GET /v1/checkin 不该成功（只注册了 POST），code=%d", rec.Code)
	}
}
