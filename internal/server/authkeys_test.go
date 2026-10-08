package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/pool"
)

// TestMatchAuthKey 多密钥匹配：命中 / 未命中 / 形态不符。
func TestMatchAuthKey(t *testing.T) {
	keys := []AuthKey{
		{Key: "sk-main", Name: "主密钥"},
		{Key: "sk-internal", Name: "内部", Groups: []string{"internal"}},
		{Key: "sk-external", Name: "外部", Groups: []string{"external"}},
	}
	cases := []struct {
		name  string
		authz string
		want  string // 期望命中条目的 Name；空 = 期望未命中
	}{
		{"命中主密钥", "Bearer sk-main", "主密钥"},
		{"命中内部密钥", "Bearer sk-internal", "内部"},
		{"命中外部密钥", "Bearer sk-external", "外部"},
		{"未知密钥", "Bearer sk-nope", ""},
		{"缺 Bearer 前缀", "sk-internal", ""},
		{"空 Authorization", "", ""},
		{"Basic 形态（非 Bearer）", "Basic dXNlcjpwYXNz", ""},
		{"前缀正确但密钥为空", "Bearer ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchAuthKey(keys, c.authz)
			if c.want == "" {
				if got != nil {
					t.Errorf("期望未命中，实得 %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("期望命中 %q，实得 nil", c.want)
			}
			if got.Name != c.want {
				t.Errorf("命中 Name=%q want %q", got.Name, c.want)
			}
		})
	}
}

// TestMatchAuthKeyNoShortCircuit 命中位置不影响结果（防"命中即 return"的时序泄漏回归）。
//
// 这里只验证语义层面：无论目标密钥排在表首还是表尾，都能正确命中，且返回的是**副本**
// （改动返回值不得影响原表）。
func TestMatchAuthKeyNoShortCircuit(t *testing.T) {
	tail := []AuthKey{
		{Key: "sk-a", Name: "A"},
		{Key: "sk-b", Name: "B"},
		{Key: "sk-target", Name: "目标", Groups: []string{"internal"}},
	}
	got := matchAuthKey(tail, "Bearer sk-target")
	if got == nil || got.Name != "目标" {
		t.Fatalf("表尾密钥未命中: %+v", got)
	}
	// 返回副本：改返回值不得污染入参表
	got.Groups[0] = "mutated"
	if tail[2].Groups[0] != "internal" {
		t.Error("matchAuthKey 返回了表内元素的引用（应为副本）")
	}
}

// TestWithAuthMultiKey 多密钥经 HTTP 层：各密钥均放行，未知密钥 401，且命中后
// 密钥条目被注入 context。
func TestWithAuthMultiKey(t *testing.T) {
	keys := []AuthKey{
		{Key: "sk-main", Name: "主密钥"},
		{Key: "sk-internal", Name: "内部", Groups: []string{"internal"}},
	}
	cases := []struct {
		name       string
		authz      string
		wantStatus int
		wantGroups []string // 期望 context 里带出的分组
	}{
		{"主密钥放行", "Bearer sk-main", http.StatusOK, nil},
		{"分组密钥放行并带出分组", "Bearer sk-internal", http.StatusOK, []string{"internal"}},
		{"未知密钥 401", "Bearer sk-other", http.StatusUnauthorized, nil},
		{"无鉴权头 401", "", http.StatusUnauthorized, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewHandler(Config{Pool: pool.New(""), AuthKeys: keys})
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if c.authz != "" {
				req.Header.Set("Authorization", c.authz)
			}
			rec := httptest.NewRecorder()

			var seenGroups []string
			var sawKey bool
			h.withAuth(func(w http.ResponseWriter, r *http.Request) {
				if ki := authKeyFrom(r.Context()); ki != nil {
					sawKey = true
					seenGroups = ki.Groups
				}
				w.WriteHeader(http.StatusOK)
			})(rec, req)

			if rec.Code != c.wantStatus {
				t.Fatalf("status=%d want %d（body=%s）", rec.Code, c.wantStatus, rec.Body.String())
			}
			if c.wantStatus != http.StatusOK {
				if sawKey {
					t.Error("鉴权失败时不该往 context 注入密钥")
				}
				return
			}
			if !sawKey {
				t.Fatal("鉴权成功时必须把命中密钥注入 context（选号链路依赖它）")
			}
			if len(seenGroups) != len(c.wantGroups) {
				t.Fatalf("context 分组=%v want %v", seenGroups, c.wantGroups)
			}
			for i := range c.wantGroups {
				if seenGroups[i] != c.wantGroups[i] {
					t.Errorf("context 分组=%v want %v", seenGroups, c.wantGroups)
				}
			}
		})
	}
}

// TestWithAuthFallsBackToAPIKey 只配 APIKey（老部署/测试路径）时行为与改动前一致。
func TestWithAuthFallsBackToAPIKey(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), APIKey: "sk-old"})

	ok := false
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-old")
	rec := httptest.NewRecorder()
	h.withAuth(func(w http.ResponseWriter, r *http.Request) { ok = true; w.WriteHeader(http.StatusOK) })(rec, req)
	if !ok || rec.Code != http.StatusOK {
		t.Fatalf("旧单密钥路径应放行，got code=%d ok=%v", rec.Code, ok)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer sk-wrong")
	rec2 := httptest.NewRecorder()
	h.withAuth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("错误密钥应 401，got %d", rec2.Code)
	}
}

// TestWithAuthNoKeysMeansOpen 不配任何密钥 = 不鉴权部署（现状语义，不得回归成 401）。
func TestWithAuthNoKeysMeansOpen(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	h.withAuth(func(w http.ResponseWriter, r *http.Request) {
		if ki := authKeyFrom(r.Context()); ki != nil {
			t.Errorf("不鉴权部署不该有密钥条目: %+v", ki)
		}
		w.WriteHeader(http.StatusOK)
	})(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("不鉴权部署应放行，got %d", rec.Code)
	}
}

// TestAuthKeyTablePrecedence AuthKeys 非空时优先于 APIKey。
func TestAuthKeyTablePrecedence(t *testing.T) {
	h := NewHandler(Config{
		Pool:     pool.New(""),
		APIKey:   "sk-legacy",
		AuthKeys: []AuthKey{{Key: "sk-new", Name: "新表"}},
	})
	table := h.authKeyTable()
	if len(table) != 1 || table[0].Key != "sk-new" {
		t.Fatalf("应优先使用 AuthKeys，实得 %+v", table)
	}
	if !strings.HasPrefix(h.authKeyTable()[0].Key, "sk-") {
		t.Error("密钥前缀异常")
	}
}
