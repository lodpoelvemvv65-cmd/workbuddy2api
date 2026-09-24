// cors_test.go 浏览器跨源直连网关的契约测试。
//
// 背景：网关不内嵌面板，但社区 GUI / 自建网页端会从别的源直接 fetch 网关；
// 没有 CORS 头时浏览器在预检阶段就拦掉，前端只能显示「无法连接模型服务」。
// 这里锁住三件事：预检回 204、正常响应带 Allow-Origin、鉴权失败的响应同样带
// Allow-Origin（否则浏览器读不到 401 信封，只会显示网络错误）。
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestCORSPreflight 预检：任意路径的 OPTIONS 都回 204 + 完整 CORS 头，
// 且回显请求声明的 Access-Control-Request-Headers。
func TestCORSPreflight(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k"})

	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://panel.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight code=%d want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin=%q want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "authorization,content-type" {
		t.Fatalf("Allow-Headers=%q want 回显请求头", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("Allow-Methods 不应为空")
	}
}

// TestCORSOnNormalResponse 普通响应（含无鉴权的 /healthz）也带 Allow-Origin。
func TestCORSOnNormalResponse(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k"})

	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("Origin", "http://panel.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin=%q want *", got)
	}
}

// TestCORSOnAuthError 鉴权失败的 401 也必须带 Allow-Origin：否则浏览器读不到
// 错误体，前端无法区分「key 错」和「网络不通」。
func TestCORSOnAuthError(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k"})

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Origin", "http://panel.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("401 Allow-Origin=%q want *", got)
	}
}
