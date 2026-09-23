package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeListen(t *testing.T) {
	cases := map[string]string{
		"":                "http://127.0.0.1:7863",
		":7863":           "http://127.0.0.1:7863",
		"0.0.0.0:9000":    "http://127.0.0.1:9000",
		"127.0.0.1:7777":  "http://127.0.0.1:7777",
		"7863":            "http://127.0.0.1:7863",
		"[::]:7863":       "http://127.0.0.1:7863", // IPv6 通配收敛到回环
		"[::1]:7863":      "http://[::1]:7863",
		"192.168.1.10:80": "http://192.168.1.10:80",
	}
	for in, want := range cases {
		if got := normalizeListen(in); got != want {
			t.Errorf("normalizeListen(%q)=%q want %q", in, got, want)
		}
	}
}

// TestProxyInjectsAuthAndForwards 验证面板代理：注入 Bearer key、转发查询串、
// 原样回传网关状态码与 body（密钥不下发浏览器由服务端代理形态保证）。
func TestProxyInjectsAuthAndForwards(t *testing.T) {
	var gotAuth, gotPath, gotQuery string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer gw.Close()

	d := &dashboard{gateway: gw.URL, apiKey: "secret", client: gw.Client()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/logs?limit=5", nil)
	d.proxy(rec, req, "/v1/logs")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth header=%q want Bearer secret", gotAuth)
	}
	if gotPath != "/v1/logs" || gotQuery != "limit=5" {
		t.Errorf("forwarded %q?%q want /v1/logs?limit=5", gotPath, gotQuery)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// TestProxyUpstreamErrorPassthrough 网关非 2xx 时状态码原样透出（页面据此提示）。
func TestProxyUpstreamErrorPassthrough(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such endpoint"}`))
	}))
	defer gw.Close()

	d := &dashboard{gateway: gw.URL, client: gw.Client()}
	rec := httptest.NewRecorder()
	d.proxy(rec, httptest.NewRequest("GET", "/api/logs", nil), "/v1/logs")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
}

func TestHandleIndex(t *testing.T) {
	d := &dashboard{}
	rec := httptest.NewRecorder()
	d.handleIndex(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "WorkBuddy2API 面板") {
		t.Errorf("index body unexpected")
	}
	// 非根路径 → 404（不把任意路径当首页）。
	rec = httptest.NewRecorder()
	d.handleIndex(rec, httptest.NewRequest("GET", "/foo", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-root code=%d want 404", rec.Code)
	}
}
