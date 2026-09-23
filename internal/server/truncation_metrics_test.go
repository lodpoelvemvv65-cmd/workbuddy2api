package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestUpstreamTruncationCounter 验证两协议在「上游 EOF 且无 finish_reason」时
// 都把断流记进 /status 计数（流式 failInterrupted），且不改变各自的协议行为。
func TestUpstreamTruncationCounter(t *testing.T) {
	withChatLog(t)
	// 只发内容帧，直接 EOF：无 finish_reason、无 [DONE]。
	frames := strings.Join([]string{
		`data: {"id":"c1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"half"},"finish_reason":null}]}`,
		``,
	}, "\n\n")
	up, _ := captureUpstream(t, 200, frames, true)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	before := upstreamTruncationSnapshot()

	recM := httptest.NewRecorder()
	h.ServeHTTP(recM, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	recR := httptest.NewRecorder()
	h.ServeHTTP(recR, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"input":"hi"}`)))

	after := upstreamTruncationSnapshot()
	if got := after["messages"] - before["messages"]; got != 1 {
		t.Errorf("messages 断流计数增量 = %d, want 1", got)
	}
	if got := after["responses"] - before["responses"]; got != 1 {
		t.Errorf("responses 断流计数增量 = %d, want 1", got)
	}

	if !strings.Contains(recM.Body.String(), "event: error") {
		t.Errorf("messages 断流应收 error 事件，body:\n%s", recM.Body.String())
	}
	if !strings.Contains(recR.Body.String(), `"reason":"upstream_interrupted"`) {
		t.Errorf("responses 断流应报 incomplete，body:\n%s", recR.Body.String())
	}
}
