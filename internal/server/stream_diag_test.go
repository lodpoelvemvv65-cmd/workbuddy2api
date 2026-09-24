package server

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// captureLogWriter 重定向标准 logger 并捕获 fn 期间的全部 log.Printf 输出。
// 只用于非并行测试：标准 logger 是进程级全局状态。
func captureLogWriter(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(flags)
	})
	fn()
	return buf.String()
}

// TestStreamEndedWithoutUsageLogsDiagnostics 回归断流观测缺口：「上游掐流」与
// 「客户端自己退出 / 空闲掐流」在协议层都表现为「无 finish_reason 的断流计数」，
// 事后无法区分。缺 usage 的流收尾必须把四要素（saw_done/saw_eof/client_gone/err）
// 一并留证，尤其是透传层此前静默丢弃的非 EOF 读错误。
func TestStreamEndedWithoutUsageLogsDiagnostics(t *testing.T) {
	// 两帧内容后连接被 RST（非 EOF 读错误），无 finish_reason、无 usage。
	frames := strings.Join([]string{
		`data: {"id":"c1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"half"},"finish_reason":null}]}`,
		``,
	}, "\n\n")
	c := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(io.MultiReader(
					strings.NewReader(frames),
					errReader{errors.New("connection reset by peer")},
				)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: c,
	})

	out := captureLogWriter(t, func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
			`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	})

	for _, want := range []string{
		"stream ended without usage",
		"saw_done=false",
		"saw_eof=false",
		"client_gone=false",
		"connection reset by peer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("断流诊断日志缺 %q，got:\n%s", want, out)
		}
	}
}
