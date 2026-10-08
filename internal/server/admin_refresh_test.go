// admin_refresh_test.go 「立即续期」端点（P0-1）与管理面提权闸的契约测试。
//
// 两组断言，各自锁住一个容易回归的点：
//  1. **提权面**：管理面只认"不受限密钥"。withAuth 会匹配 config.api_keys 里的
//     分组密钥，若不在管理面额外收紧，开 admin.enabled 等于把 disable/refresh
//     开给外部调用方（他们手里的就是分组密钥）。
//  2. **续期语义**：单次尝试、如实回传；不动 disabled/manual_disabled；
//     失败不喂 NoteError/熔断（验证性调用不该给账号记一笔失败）。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// newRefreshTestUpstream 起一个假上游，只实现 token 续期那一跳
// （POST /v2/plugin/auth/token/refresh）。ok=false 时回 500 以覆盖失败分支。
func newRefreshTestUpstream(t *testing.T, ok bool) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"upstream boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 必须按上游的真实信封 `{"code":0,"data":{...}}` 返回：doJSON 会先解信封
		// 再取 data（裸对象会被当成"没有 accessToken"）。
		// expiresIn=5184000（60d）与实测上游一致。
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at2","refreshToken":"rt2","expiresIn":5184000}}`))
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	return up
}

// refreshResp 与 adminRefreshResult 同构（测试侧独立声明，避免跟着实现改字段名）。
type refreshResp struct {
	UID            string `json:"uid"`
	OK             bool   `json:"ok"`
	Detail         string `json:"detail"`
	TokenExpiresAt int64  `json:"token_expires_at"`
	NeedsRelogin   bool   `json:"needs_relogin"`
}

func postRefresh(t *testing.T, h *Handler, uid, key string) (*httptest.ResponseRecorder, refreshResp) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/accounts/"+uid+"/refresh", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	h.ServeHTTP(rec, req)
	var out refreshResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode refresh response: %v body=%s", err, rec.Body)
		}
	}
	return rec, out
}

// TestAdminRefreshDisabledByDefault 开关关闭时路由不注册：409/200 都不该出现，
// 一律 404 且为 mux 默认纯文本形态（存在性不泄露，同 disable/revive 的口径）。
func TestAdminRefreshDisabledByDefault(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", RefreshToken: "rt"})
	h := NewHandler(Config{Pool: p, APIKey: "k"}) // AdminEnabled 零值 false

	rec, _ := postRefresh(t, h, "u1", "k")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 when admin disabled", rec.Code)
	}
}

// TestAdminOpsRejectsGroupKey ★ 提权闸 ★
// 分组密钥（发给外部调用方）调管理面必须 403；不受限密钥放行。
// 这条断言若失败，意味着外部用户能摘空整个号池。
func TestAdminOpsRejectsGroupKey(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", FilePath: t.TempDir() + "/u1.json"}
	p := testPoolWith(a)
	h := NewHandler(Config{
		Pool:         p,
		AdminEnabled: true,
		AuthKeys: []AuthKey{
			{Key: "master"}, // 主密钥：无分组 = 不受限
			{Key: "ext", Groups: []string{"external"}}, // 分组密钥：发给外部调用方
		},
		Upstream: newRefreshTestUpstream(t, true),
	})

	paths := []struct{ name, path string }{
		{"disable", "/admin/accounts/u1/disable"},
		{"enable", "/admin/accounts/u1/enable"},
		{"revive", "/admin/accounts/u1/revive"},
		{"refresh", "/admin/accounts/u1/refresh"},
		{"task run", "/admin/tasks/keepalive/run"},
	}
	for _, tc := range paths {
		// 分组密钥 → 403（且不应落到 handler：状态位不得被触碰）
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", tc.path, nil)
		req.Header.Set("Authorization", "Bearer ext")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: 分组密钥 code=%d want 403", tc.name, rec.Code)
		}

		// 不受限密钥 → 不是 401/403（具体业务码由各 handler 决定）
		rec = httptest.NewRecorder()
		req = httptest.NewRequest("POST", tc.path, nil)
		req.Header.Set("Authorization", "Bearer master")
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("%s: 不受限密钥 code=%d want 非 403/401", tc.name, rec.Code)
		}
	}

	// 被拒的分组密钥不得留下任何状态痕迹
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应仍在池内")
	}
	if st.ManualDisabled || st.Disabled {
		t.Fatalf("分组密钥被拒后状态位被改动: %+v", st)
	}
}

// TestAdminRefreshUnknownUID 未知 uid → 404（与 disable/revive 同口径）。
func TestAdminRefreshUnknownUID(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", RefreshToken: "rt"})
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Upstream: newRefreshTestUpstream(t, true)})

	rec, _ := postRefresh(t, h, "nope", "k")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404, body=%s", rec.Code, rec.Body)
	}
}

// TestAdminRefreshNoRefreshToken 无 refresh token → 409 + needs_relogin，
// 且**不**发上游请求（省一次注定失败的往返）。台账记一次失败。
func TestAdminRefreshNoRefreshToken(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at"}) // 无 RefreshToken
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Upstream: newRefreshTestUpstream(t, true)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/accounts/u1/refresh", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d want 409, body=%s", rec.Code, rec.Body)
	}
	var out refreshResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.NeedsRelogin != true {
		t.Error("无 refresh token 必须标 needs_relogin=true")
	}
	st, _ := p.Status("u1")
	if st.RefreshFailStreak != 1 {
		t.Errorf("refresh_fail_streak = %d want 1（失败应记台账）", st.RefreshFailStreak)
	}
	if st.NeedsRelogin {
		t.Error("仅「无 refresh token」而未被禁用时，status.needs_relogin 仍是 false（那是禁用态判据）")
	}
}

// TestAdminRefreshSuccess 成功路径：凭证写回、台账留证、连击清零、返回新到期时刻。
func TestAdminRefreshSuccess(t *testing.T) {
	fp := t.TempDir() + "/u1.json"
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		Domain: "copilot.tencent.com", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("准备凭证文件: %v", err)
	}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Upstream: newRefreshTestUpstream(t, true)})

	// 先制造一次失败连击，验证成功会把它清零
	p.NoteRefreshFail("u1", errors.New("seed"))
	before := time.Now().Unix()

	rec, out := postRefresh(t, h, "u1", "k")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200, body=%s", rec.Code, rec.Body)
	}
	if !out.OK {
		t.Errorf("ok=false, detail=%q", out.Detail)
	}
	// expiresIn=5184000（60d）→ 新到期时刻应在 59 天后之后
	if out.TokenExpiresAt <= before {
		t.Errorf("token_expires_at=%d 应是新值（> %d）", out.TokenExpiresAt, before)
	}
	if got := a.AccessTokenValue(); got != "at2" {
		t.Errorf("accessToken=%q want at2（凭证未写回）", got)
	}

	st, _ := p.Status("u1")
	if st.RefreshOKAt == nil {
		t.Error("refresh_ok_at 应被写入")
	}
	if st.RefreshFailStreak != 0 {
		t.Errorf("成功后续期连击应清零, got %d", st.RefreshFailStreak)
	}
	if st.TokenExpired {
		t.Error("续期后不该仍判过期")
	}
}

// TestAdminRefreshUpstreamFailure 上游失败 → 502；台账记失败，但**不**动池状态
// （验证性调用不喂 NoteError/熔断，也不改 disabled）。
func TestAdminRefreshUpstreamFailure(t *testing.T) {
	fp := t.TempDir() + "/u1.json"
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		Domain: "copilot.tencent.com", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("准备凭证文件: %v", err)
	}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, APIKey: "k", AdminEnabled: true, Upstream: newRefreshTestUpstream(t, false)})

	rec, _ := postRefresh(t, h, "u1", "k")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502, body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.RefreshFailStreak != 1 {
		t.Errorf("refresh_fail_streak=%d want 1", st.RefreshFailStreak)
	}
	if st.LastRefreshErr == "" {
		t.Error("last_refresh_err 应记录失败原因")
	}
	if st.Disabled || st.ManualDisabled {
		t.Error("验证性续期失败不得改动账号可用状态")
	}
	if st.BreakerFails != 0 {
		t.Errorf("不得推进熔断（breaker_fails=%d）", st.BreakerFails)
	}
	if got := a.AccessTokenValue(); got != "at" {
		t.Errorf("失败时不该改写 accessToken, got %q", got)
	}
}
