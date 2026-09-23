package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// captureUpstream 假上游：记录实际收到的请求体，便于断言出站翻译结果（尤其是
// conversation_id 的接线——缓存契约的落点）。
func captureUpstream(t *testing.T, status int, body string, isStream bool) (*upstream.Client, *string) {
	t.Helper()
	got := ""
	c := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return c, &got
}

// TestAnthropicToOpenAIRequest 请求翻译：system / 多模态 / 工具往返 / 采样参数。
func TestAnthropicToOpenAIRequest(t *testing.T) {
	in := []byte(`{
	  "model":"claude-sonnet-4-5",
	  "max_tokens":1024,
	  "temperature":0.3,
	  "top_p":0.9,
	  "stop_sequences":["END"],
	  "system":[{"type":"text","text":"be nice"}],
	  "metadata":{"user_id":"user_x_account_y_session_z"},
	  "thinking":{"type":"enabled","budget_tokens":9000},
	  "tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
	  "tool_choice":{"type":"tool","name":"get_weather"},
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
	    {"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SH"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"sunny"}]}]}
	  ]
	}`)
	out, meta, err := anthropicToOpenAI(in, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	// claude* → 网关默认模型（上游没有 claude 名）。
	if obj["model"] != "deepseek-v4.1-flash" {
		t.Errorf("model = %v, want deepseek-v4.1-flash（claude* 落到配置的默认模型）", obj["model"])
	}
	// 缓存契约：metadata.user_id 必须落到出站 conversation_id。
	if obj["conversation_id"] != "user_x_account_y_session_z" {
		t.Errorf("conversation_id = %v, want user_id 原值", obj["conversation_id"])
	}
	if meta.ConversationID != "user_x_account_y_session_z" {
		t.Errorf("meta.ConversationID = %q", meta.ConversationID)
	}
	if meta.Model != "claude-sonnet-4-5" {
		t.Errorf("meta.Model 应回显客户端原模型名, got %q", meta.Model)
	}
	if obj["max_tokens"].(float64) != 1024 || obj["temperature"].(float64) != 0.3 {
		t.Errorf("采样参数丢失: %v", obj)
	}
	// tool_choice=any/tool 语义映射。
	tcm, _ := obj["tool_choice"].(map[string]any)
	if tcm["type"] != "function" {
		t.Errorf("tool_choice = %v", obj["tool_choice"])
	}
	// stop_sequences → stop。
	stop, _ := obj["stop"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v", obj["stop"])
	}
	// thinking: enabled → 出站 thinking + effort（budget 9000 → medium）。
	tk, _ := obj["thinking"].(map[string]any)
	if tk["type"] != "enabled" {
		t.Errorf("thinking = %v", obj["thinking"])
	}
	if obj["reasoning_effort"] != "medium" {
		t.Errorf("reasoning_effort = %v, want medium", obj["reasoning_effort"])
	}
	// 消息序列：system → user(parts 含 image_url) → assistant(tool_calls) → tool。
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages 条数 = %d, want 4: %v", len(msgs), msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be nice" {
		t.Errorf("system 消息 = %v", sys)
	}
	user, _ := msgs[1].(map[string]any)
	parts, _ := user["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("user parts = %v", user["content"])
	}
	img, _ := parts[1].(map[string]any)
	iu, _ := img["image_url"].(map[string]any)
	if img["type"] != "image_url" || !strings.HasPrefix(iu["url"].(string), "data:image/png;base64,") {
		t.Errorf("image part = %v", img)
	}
	asst, _ := msgs[2].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls = %v", asst)
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || !strings.Contains(fn["arguments"].(string), "SH") {
		t.Errorf("tool_call = %v", tcs[0])
	}
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" || toolMsg["content"] != "sunny" {
		t.Errorf("tool_result 消息 = %v", toolMsg)
	}
}

// TestAnthropicThinkingBlocksDroppedOnInput 入站 thinking 块丢弃（上游不认该形态）。
func TestAnthropicThinkingBlocksDroppedOnInput(t *testing.T) {
	in := []byte(`{"model":"auto","messages":[{"role":"assistant","content":[
	  {"type":"thinking","thinking":"secret","signature":"sig"},{"type":"text","text":"answer"}]}]}`)
	out, _, err := anthropicToOpenAI(in, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if strings.Contains(string(out), "secret") {
		t.Errorf("thinking 块应被丢弃, got %s", out)
	}
	if !strings.Contains(string(out), "answer") {
		t.Errorf("正文必须保留, got %s", out)
	}
}

// TestAnthropicUsageCacheSemantics Anthropic 的 input_tokens 不含缓存命中部分。
func TestAnthropicUsageCacheSemantics(t *testing.T) {
	u := map[string]any{
		"prompt_tokens": float64(2036), "completion_tokens": float64(55),
		"prompt_cache_hit_tokens": float64(1792), "credit": float64(0.03),
	}
	got := anthropicUsage(u, 0)
	if got["input_tokens"] != 244 {
		t.Errorf("input_tokens = %v, want 244 (2036-1792)", got["input_tokens"])
	}
	if got["cache_read_input_tokens"] != 1792 {
		t.Errorf("cache_read_input_tokens = %v, want 1792", got["cache_read_input_tokens"])
	}
	if got["output_tokens"] != 55 {
		t.Errorf("output_tokens = %v", got["output_tokens"])
	}
}

// TestAnthropicNonStreamResponse 非流式响应翻译（含 tool_use 与 stop_reason）。
func TestAnthropicNonStreamResponse(t *testing.T) {
	resp := map[string]any{
		"id": "chatcmpl-1", "model": "deepseek-v4-flash",
		"choices": []any{map[string]any{
			"index": float64(0), "finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant", "content": "let me check",
				"reasoning_content": "need a tool",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "get_weather", "arguments": `{"city":"SH"}`},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(4)},
	}
	got := openAIToAnthropicMessage(resp, anthroMeta{Model: "claude-sonnet-4-5"})
	if got["type"] != "message" || got["role"] != "assistant" {
		t.Fatalf("message 信封 = %v", got)
	}
	if got["model"] != "claude-sonnet-4-5" {
		t.Errorf("model 应回显客户端模型名, got %v", got["model"])
	}
	if got["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", got["stop_reason"])
	}
	blocks, _ := got["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("content blocks = %v", blocks)
	}
	if b, _ := blocks[0].(map[string]any); b["type"] != "thinking" {
		t.Errorf("block0 = %v, want thinking", blocks[0])
	}
	if b, _ := blocks[1].(map[string]any); b["type"] != "text" || b["text"] != "let me check" {
		t.Errorf("block1 = %v", blocks[1])
	}
	tu, _ := blocks[2].(map[string]any)
	input, _ := tu["input"].(map[string]any)
	if tu["type"] != "tool_use" || tu["name"] != "get_weather" || input["city"] != "SH" {
		t.Errorf("block2 = %v", blocks[2])
	}
}

// TestAnthropicStreamEventSequence 流式：OpenAI SSE 帧 → Anthropic 事件时序。
func TestAnthropicStreamEventSequence(t *testing.T) {
	withChatLog(t)
	frames := strings.Join([]string{
		`data: {"id":"chatcmpl-9","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{"reasoning_content":"hmm"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":8}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	up, _ := captureUpstream(t, 200, frames, true)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"metadata":{"user_id":"sess-1"},"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// 事件顺序（严格递增的下标断言）。
	order := []string{
		"event: message_start", `"type":"thinking"`, `"type":"thinking_delta"`,
		"event: content_block_stop", `"type":"text"`, `"text_delta"`, `"text":" there"`,
		"event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop",
	}
	pos := 0
	for _, want := range order {
		i := strings.Index(body[pos:], want)
		if i < 0 {
			t.Fatalf("事件序列缺少 %q\n--- 实际输出 ---\n%s", want, body)
		}
		pos += i + len(want)
	}
	// 缓存数字透出：input_tokens = 10-8 = 2。
	if !strings.Contains(body, `"input_tokens":2`) {
		t.Errorf("message_delta 应带真实 input_tokens=2（扣除缓存命中）:\n%s", body)
	}
	if !strings.Contains(body, `"cache_read_input_tokens":8`) {
		t.Errorf("应透出 cache_read_input_tokens=8:\n%s", body)
	}
	if !strings.Contains(body, `"output_tokens":2`) {
		t.Errorf("应透出 output_tokens=2:\n%s", body)
	}
	// Anthropic 事件必须带 event: 行（SDK 按此分派）。
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("OpenAI 的 [DONE] 不应出现在 Anthropic 流里")
	}
}

// TestAnthropicEndpointInjectsConversationID 端到端：出站 body 必须带
// conversation_id（粘性 + prompt_cache_key 的输入），这是缓存不被打散的前提。
func TestAnthropicEndpointInjectsConversationID(t *testing.T) {
	withChatLog(t)
	// 注意：网关对上游**恒走 SSE**（非流式是本地 Aggregate 聚合），故假上游也必须
	// 回 SSE 帧——回单块 JSON 会被判成 "upstream stream contained no valid data events"。
	resp := strings.Join([]string{
		`data: {"id":"c1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	up, got := captureUpstream(t, 200, resp, true)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "k1"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"metadata":{"user_id":"sess-42"},"messages":[{"role":"user","content":"hi"}]}`))
	// Anthropic 客户端只发 x-api-key（不发 Authorization）——鉴权必须接受该形态。
	req.Header.Set("X-Api-Key", "k1")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("x-api-key 鉴权失败: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*got, `"conversation_id":"sess-42"`) {
		t.Errorf("出站 body 必须带 conversation_id（缓存键/粘性输入），got: %s", *got)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 Anthropic JSON: %s", rec.Body.String())
	}
	if out["type"] != "message" {
		t.Errorf("响应信封 = %v", out)
	}
}

// TestAnthropicAuthErrorShape 401 必须是 Anthropic 错误信封（客户端认 error.type）。
func TestAnthropicAuthErrorShape(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, `{}`, false)
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, APIKey: "k1"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"auto","messages":[]}`)))
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Errorf("/v1/messages 的 401 应是 Anthropic 信封, got %s", rec.Body.String())
	}
}

// TestAnthropicCountTokens 计数端点：返回 Anthropic 的 {"input_tokens":N}。
func TestAnthropicCountTokens(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, `{}`, false)
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"auto","messages":[{"role":"user","content":"hello world"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %s", rec.Body.String())
	}
	if n, ok := out["input_tokens"].(float64); !ok || n <= 0 {
		t.Errorf("input_tokens = %v, want > 0", out["input_tokens"])
	}
}

// TestAnthropicNonStreamErrorEnvelope 上游错误 → Anthropic 错误信封（原文保留）。
func TestAnthropicNonStreamErrorEnvelope(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 500, `{"code":11128,"msg":"upstream boom","requestId":"r1"}`, false)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %s", rec.Body.String())
	}
	if out["type"] != "error" {
		t.Fatalf("信封 = %v (code=%d)", out, rec.Code)
	}
	e, _ := out["error"].(map[string]any)
	if e["type"] == "" || e["message"] == "" {
		t.Errorf("error 信封不完整: %v", out)
	}
}

// TestResolveAnthropicModel 模型名决策：透传优先 / 剥上下文标记 / claude* 兜底 / realm 保真。
func TestResolveAnthropicModel(t *testing.T) {
	const def = "deepseek-v4.1-flash"
	cases := []struct {
		name, in, def, want string
	}{
		{"claude* 落默认", "claude-sonnet-4-5", def, def},
		{"claude* 带 [1m] 标记", "claude-sonnet-4-5[1m]", def, def},
		{"claude* 国际版落 global 默认", "global:claude-sonnet-4-5", def, "global:" + def},
		{"claude* 国际版+标记", "global:claude-sonnet-4-5[1m]", def, "global:" + def},
		{"配置自带 global 前缀则以配置为准", "claude-3-5-haiku", "global:" + def, "global:" + def},
		{"透传真实模型名", "deepseek-v4.1-flash", def, def},
		{"透传并剥 [1m]（上游不认该后缀）", "deepseek-v4-flash[1m]", def, "deepseek-v4-flash"},
		{"透传并剥 [200k]", "glm-5.3[200k]", def, "glm-5.3"},
		{"透传保留 realm 前缀", "global:glm-5.3", def, "global:glm-5.3"},
		{"透传+剥后缀+保留 realm", "global:kimi-k2.8-preview[1m]", def, "global:kimi-k2.8-preview"},
		{"配置为空回落内置默认", "claude-sonnet-4-5", "", defaultAnthropicModel},
		{"空模型名", "", def, ""},
	}
	for _, c := range cases {
		if got := resolveAnthropicModel(c.in, c.def); got != c.want {
			t.Errorf("%s: resolveAnthropicModel(%q, %q) = %q, want %q", c.name, c.in, c.def, got, c.want)
		}
	}
}

// TestStripContextMarker 后缀剥离只认「数字+可选单位」形态，不误伤正常模型名。
func TestStripContextMarker(t *testing.T) {
	cases := map[string]string{
		"deepseek-v4.1-flash[1m]": "deepseek-v4.1-flash",
		"gpt-5.6-luna[1M]":        "gpt-5.6-luna",
		"glm-5.3[200k]":           "glm-5.3",
		"model[1000000]":          "model",
		"deepseek-v4.1-flash":     "deepseek-v4.1-flash",
		"not-a-model[abc]":        "not-a-model[abc]", // 非数字标记不剥
		"[1m]":                    "[1m]",             // 纯标记不剥（无模型名）
	}
	for in, want := range cases {
		if got := stripContextMarker(in); got != want {
			t.Errorf("stripContextMarker(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAnthropicStreamUpstreamInterrupted 上游断流（无 finish_reason）不得谎报正常结束。
//
// 回归锚：上游连接被掐（只发内容、没有 finish_reason 帧）时，此前 finalize() 会补一个
// stop_reason:"end_turn" + message_stop —— 而 Anthropic 的 end_turn 语义是「模型自然
// 说完」，客户端据此把半截正文当**最终答案**采纳：既不重试也不报错。用户观感即
// 「输出到一半突然没了、却显示正常结束」。
//
// 官方对「流中途失败」的形态是 error 事件（SDK 收到即抛错，客户端走重试/报错路径），
// 故此处断言：必须发 event: error，且**不得**出现任何表示正常结束的收尾。
func TestAnthropicStreamUpstreamInterrupted(t *testing.T) {
	withChatLog(t)
	// 只有内容帧，直接 EOF：无 finish_reason、无 [DONE]。
	up, _ := captureUpstream(t, 200, strings.Join([]string{
		`data: {"id":"c1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"前半段"},"finish_reason":null}]}`,
		``,
	}, "\n\n"), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	// 断流前的内容仍要交出去（客户端可见已生成部分），但不能被声明为"说完了"。
	if !strings.Contains(body, "前半段") {
		t.Errorf("断流前的内容应保留:\n%s", body)
	}
	if !strings.Contains(body, "event: error") {
		t.Errorf("上游断流必须发 error 事件（官方流中失败形态）:\n%s", body)
	}
	if strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Errorf("上游断流不得报 end_turn（客户端会把半截正文当最终答案）:\n%s", body)
	}
	// error 之前必须补 content_block_stop：否则客户端留下未闭合块，流尾结构不完整。
	stopIdx := strings.Index(body, "event: content_block_stop")
	errIdx := strings.Index(body, "event: error")
	if stopIdx < 0 || errIdx < 0 || stopIdx > errIdx {
		t.Errorf("error 前应先发 content_block_stop（闭合未完成块）:\n%s", body)
	}
}

// TestAnthropicStreamLengthTruncation 被 max_tokens 截断 → stop_reason:"max_tokens"。
//
// 与 end_turn 的区别很关键：max_tokens 让客户端知道「是输出上限截断的」，可提示用户
// 调大上限；此前 finish("length") 已经走 anthropicStopReason 翻译，本用例把它锚住。
func TestAnthropicStreamLengthTruncation(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"被截断的"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":64}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	if !strings.Contains(body, `"stop_reason":"max_tokens"`) {
		t.Errorf("length 应译为 stop_reason=max_tokens:\n%s", body)
	}
	if strings.Contains(body, "event: error") {
		t.Errorf("max_tokens 是正常收尾（有 finish_reason），不应发 error:\n%s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("正常收尾应发 message_stop:\n%s", body)
	}
}

// TestAnthropicStreamNormalFinishUnchanged 正常收尾（finish_reason + [DONE] 齐备）零回归。
func TestAnthropicStreamNormalFinishUnchanged(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	if !strings.Contains(body, `"stop_reason":"end_turn"`) || !strings.Contains(body, "event: message_stop") {
		t.Errorf("正常收尾应保持 end_turn + message_stop:\n%s", body)
	}
	if strings.Contains(body, "event: error") {
		t.Errorf("正常收尾不得出现 error 事件:\n%s", body)
	}
}

// TestAnthropicNonStreamUpstreamInterrupted 非流式：上游断流必须回错误，不得编造完整消息。
//
// 非流式路径此刻还没向客户端写过任何字节，最诚实的处置是明确报错；此前它会用
// finish_reason 的默认值 "end_turn" 造一条"完整"消息，把半截正文当最终答案。
func TestAnthropicNonStreamUpstreamInterrupted(t *testing.T) {
	withChatLog(t)
	// 内容帧后直接 EOF：Aggregate 见不到 finish_reason，会置截断标记。
	up, _ := captureUpstream(t, 200, strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"半截"},"finish_reason":null}]}`,
		``,
	}, "\n\n"), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	if rec.Code != 502 {
		t.Errorf("上游断流应回 502, got %d body=%s", rec.Code, body)
	}
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("应是 Anthropic 错误信封:\n%s", body)
	}
	if strings.Contains(body, `"stop_reason"`) {
		t.Errorf("不得返回 message（半截正文会被当最终答案）:\n%s", body)
	}
	// 内部标记键绝不能泄露。
	if strings.Contains(body, upstream.TruncatedKey) {
		t.Errorf("内部截断标记不得出现在响应里:\n%s", body)
	}
}

// TestAnthropicNonStreamNormalUnchanged 非流式正常收尾零回归（finish_reason 齐备）。
func TestAnthropicNonStreamNormalUnchanged(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Errorf("正常收尾应回 200 + end_turn, got %d body=%s", rec.Code, body)
	}
	if strings.Contains(body, upstream.TruncatedKey) {
		t.Errorf("内部截断标记不得出现在响应里:\n%s", body)
	}
}

// TestAnthropicToolResultImagePreserved tool_result 内的 image 块必须转成 chat 多模态
// parts（方案 D）——旧实现只取 text 块、把图静默丢掉，模型因此看不见 read 到的图片。
func TestAnthropicToolResultImagePreserved(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":100,"messages":[
	  {"role":"user","content":"q"},
	  {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
	     {"type":"text","text":"Read image file [image/png]"},
	     {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]}]}]}`)
	out, _, err := anthropicToOpenAI(in, "m")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" {
		t.Fatalf("tool msg = %v", toolMsg)
	}
	parts, ok := toolMsg["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("tool content 应为 2 个 part（text+image），got %v", toolMsg["content"])
	}
	if parts[0].(map[string]any)["type"] != "text" {
		t.Errorf("part[0] = %v", parts[0])
	}
	img, _ := parts[1].(map[string]any)
	iu, _ := img["image_url"].(map[string]any)
	if img["type"] != "image_url" || !strings.HasPrefix(iu["url"].(string), "data:image/png;base64,") {
		t.Errorf("part[1] image = %v", img)
	}
	// 纯文本 tool_result 仍须收敛成字符串（存量请求零漂移）。
	in2 := []byte(`{"model":"m","messages":[{"role":"user","content":[
	  {"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"sunny"}]}]}]}`)
	out2, _, _ := anthropicToOpenAI(in2, "m")
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	m0, _ := obj2["messages"].([]any)[0].(map[string]any)
	if m0["content"] != "sunny" {
		t.Errorf("纯文本 tool_result 应为字符串, got %v", m0["content"])
	}
}
