// cors_test.go 浏览器跨源直连网关的契约测试。
//
// 背景：网关不内嵌面板，但社区 GUI / 自建网页端会从别的源直接 fetch 网关；
// 没有 CORS 头时浏览器在预检阶段就拦掉，前端只能显示「无法连接模型服务」。
// 这里锁住浏览器实际会踩的几条规则：
//   - 任意路径的 OPTIONS 预检回 204，并回显请求声明的头；
//   - 带 Origin 时回显 Origin + Allow-Credentials（兼容 fetch credentials:'include'）；
//   - 无 Origin（curl/SDK）时回落 `*`；
//   - Private Network Access 预检回 Allow-Private-Network；
//   - 鉴权失败的 401 同样带 CORS 头（否则浏览器读不到错误体）。
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

func newCORSHandler() *Handler {
	return NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"}), APIKey: "k"})
}

// TestCORSPreflight 预检：任意路径的 OPTIONS 都回 204 + 完整 CORS 头，
// 且回显请求声明的 Access-Control-Request-Headers 与 Origin。
func TestCORSPreflight(t *testing.T) {
	h := newCORSHandler()

	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://panel.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight code=%d want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://panel.example" {
		t.Fatalf("Allow-Origin=%q want 回显 Origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Allow-Credentials=%q want true", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "authorization,content-type" {
		t.Fatalf("Allow-Headers=%q want 回显请求头", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("Allow-Methods 不应为空")
	}
	if got := rec.Header().Get("Vary"); got == "" {
		t.Fatal("Vary 不应为空（回显 Origin / 请求头需声明）")
	}
}

// TestCORSPrivateNetworkAccess HTTPS 页面访问本机 / 私网 HTTP 网关时，Chrome 的
// PNA 预检带 Access-Control-Request-Private-Network，服务端必须回 Allow 头。
func TestCORSPrivateNetworkAccess(t *testing.T) {
	h := newCORSHandler()

	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://webui.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Fatalf("Allow-Private-Network=%q want true", got)
	}
}

// TestCORSNoOriginFallback 无 Origin 的非浏览器请求回落 `*`，且不声明凭证。
func TestCORSNoOriginFallback(t *testing.T) {
	h := newCORSHandler()

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin=%q want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("无 Origin 时不应声明 Allow-Credentials，got %q", got)
	}
}

// TestCORSOnNormalResponse 普通响应（含无鉴权的 /healthz）也带 CORS 头。
func TestCORSOnNormalResponse(t *testing.T) {
	h := newCORSHandler()

	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("Origin", "http://panel.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://panel.example" {
		t.Fatalf("Allow-Origin=%q want 回显 Origin", got)
	}
}

// TestCORSOnAuthError 鉴权失败的 401 也必须带 CORS 头：否则浏览器读不到错误体，
// 前端无法区分「key 错」和「网络不通」。
func TestCORSOnAuthError(t *testing.T) {
	h := newCORSHandler()

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Origin", "http://panel.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://panel.example" {
		t.Fatalf("401 Allow-Origin=%q want 回显 Origin", got)
	}
}
