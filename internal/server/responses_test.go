package server

import (
	"encoding/json"
	"strings"
	"testing"

	"net/http/httptest"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// responsesSSE 一份最小可用的上游 SSE 帧序列（网关对上游恒走 SSE）。
func responsesSSE(extra ...string) string {
	frames := []string{
		`data: {"id":"c1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
	}
	frames = append(frames, extra...)
	frames = append(frames,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":80}}`,
		`data: [DONE]`,
		``,
	)
	return strings.Join(frames, "\n\n")
}

// TestResponsesToOpenAIRequest 请求翻译：instructions / 输入条目 / 工具 / reasoning。
func TestResponsesToOpenAIRequest(t *testing.T) {
	in := []byte(`{
	  "model":"deepseek-v4.1-flash",
	  "instructions":"be terse",
	  "max_output_tokens":512,
	  "stream":true,
	  "temperature":0.2,
	  "top_p":0.8,
	  "reasoning":{"effort":"xhigh","summary":"auto"},
	  "prompt_cache_key":"sess-1",
	  "tools":[
	    {"type":"function","name":"exec_command","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}},"strict":false},
	    {"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function","name":"close_agent"}]},
	    {"type":"web_search","external_web_access":true}
	  ],
	  "tool_choice":"auto",
	  "text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"},"strict":true}},
	  "input":[
	    {"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]},
	    {"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
	    {"type":"function_call","id":"fc_2","call_id":"call_2","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"a.txt"},
	    {"type":"function_call_output","call_id":"call_2","output":{"text":"/tmp"}}
	  ]
	}`)
	out, meta, err := responsesToOpenAI(in, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	if obj["model"] != "deepseek-v4.1-flash" || meta.Model != "deepseek-v4.1-flash" {
		t.Errorf("model = %v meta=%q", obj["model"], meta.Model)
	}
	if !meta.Stream {
		t.Errorf("stream 应翻译透传")
	}
	if obj["max_tokens"].(float64) != 512 || obj["temperature"].(float64) != 0.2 || obj["top_p"].(float64) != 0.8 {
		t.Errorf("采样参数丢失: %v", obj)
	}
	// reasoning.effort=xhigh → 上游 high + thinking enabled（上游只认 low/medium/high）。
	if obj["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v, want high", obj["reasoning_effort"])
	}
	if tk, _ := obj["thinking"].(map[string]any); tk["type"] != "enabled" {
		t.Errorf("thinking = %v", obj["thinking"])
	}
	// 缓存契约：客户端的 prompt_cache_key 原样透传（不做任何改写）。
	if obj["prompt_cache_key"] != "sess-1" {
		t.Errorf("prompt_cache_key 应原样透传, got %v", obj["prompt_cache_key"])
	}
	// 工具：只留 function，namespace/web_search 丢弃（上游 chat 协议无对应物）。
	tools, _ := obj["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 条数 = %d, want 1（仅 function）: %v", len(tools), tools)
	}
	tm, _ := tools[0].(map[string]any)
	fn, _ := tm["function"].(map[string]any)
	if tm["type"] != "function" || fn["name"] != "exec_command" {
		t.Errorf("tool = %v", tools[0])
	}
	if rf, _ := obj["response_format"].(map[string]any); rf["type"] != "json_schema" {
		t.Errorf("text.format → response_format 丢失: %v", obj["response_format"])
	}
	// 消息序列：system(instructions) → system(developer) → user
	//          → assistant(tool_calls×2，并簇) → tool → tool。
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("messages 条数 = %d, want 6: %v", len(msgs), msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be terse" {
		t.Errorf("instructions → 首条 system: %v", sys)
	}
	dev, _ := msgs[1].(map[string]any)
	if dev["role"] != "system" || dev["content"] != "rules" {
		t.Errorf("developer 应翻成 system: %v", dev)
	}
	user, _ := msgs[2].(map[string]any)
	if user["role"] != "user" || user["content"] != "hi" {
		t.Errorf("user 消息 = %v", user)
	}
	asst, _ := msgs[3].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("并行 function_call 应并簇进一条 assistant: %v", asst)
	}
	tc0, _ := tcs[0].(map[string]any)
	f0, _ := tc0["function"].(map[string]any)
	if tc0["id"] != "call_1" || f0["name"] != "exec_command" || !strings.Contains(f0["arguments"].(string), "ls") {
		t.Errorf("tool_call[0] = %v", tc0)
	}
	m3, _ := msgs[4].(map[string]any)
	if m3["role"] != "tool" || m3["tool_call_id"] != "call_1" || m3["content"] != "a.txt" {
		t.Errorf("function_call_output → tool 消息 = %v", m3)
	}
	m4, _ := msgs[5].(map[string]any)
	if m4["tool_call_id"] != "call_2" || m4["content"] != `{"text":"/tmp"}` {
		t.Errorf("对象型 output 应 JSON 化 = %v", m4)
	}
}

// TestResponsesReasoningItemDropped reasoning 条目丢弃（上游不认；带签名形态只在官方侧成立）。
func TestResponsesReasoningItemDropped(t *testing.T) {
	out, _, err := responsesToOpenAI([]byte(`{"model":"m","input":[
	  {"type":"reasoning","summary":[{"type":"summary_text","text":"secret"}],"encrypted_content":"zzz"},
	  {"type":"message","role":"user","content":"hi"}]}`), "m")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if strings.Contains(string(out), "secret") || strings.Contains(string(out), "encrypted_content") {
		t.Errorf("reasoning 条目不应出现在出站 body: %s", out)
	}
}

// TestResponsesInputStringForm input 为字符串 / 空指令 / 空模型等边界。
func TestResponsesInputStringForm(t *testing.T) {
	out, _, err := responsesToOpenAI([]byte(`{"input":"hi"}`), "def-model")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["model"] != "def-model" {
		t.Errorf("空模型应落默认模型, got %v", obj["model"])
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	m, _ := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "hi" {
		t.Errorf("字符串 input → user 消息, got %v", m)
	}
	if _, ok := obj["reasoning_effort"]; ok {
		t.Errorf("无 reasoning 时不应带 reasoning_effort: %v", obj)
	}
}

// TestResponsesReasoningSummaryAloneDoesNotEnableThinking effort 为空（codex 常见形态
// {"summary":"auto"}）不得开思考——否则每次请求都平白变慢。
func TestResponsesReasoningSummaryAloneDoesNotEnableThinking(t *testing.T) {
	out, _, err := responsesToOpenAI([]byte(`{"model":"m","reasoning":{"summary":"auto"},"input":"hi"}`), "m")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if _, ok := obj["thinking"]; ok {
		t.Errorf("summary-only 不应开 thinking: %v", obj)
	}
	if _, ok := obj["reasoning_effort"]; ok {
		t.Errorf("summary-only 不应带 reasoning_effort: %v", obj)
	}
}

// TestResponsesUsageIncludesCached Responses 的 input_tokens 是**含缓存**总额
// （与 Anthropic 的剔除法相反）——照抄 anthropicUsage 会让客户端上下文统计偏小。
func TestResponsesUsageIncludesCached(t *testing.T) {
	u := responsesUsage(map[string]any{
		"prompt_tokens": float64(100), "completion_tokens": float64(5),
		"cache_read_input_tokens": float64(80),
	}, 999)
	if u["input_tokens"].(int) != 100 {
		t.Errorf("input_tokens = %v, want 100（含缓存，不减）", u["input_tokens"])
	}
	d, _ := u["input_tokens_details"].(map[string]any)
	if d["cached_tokens"].(int) != 80 {
		t.Errorf("cached_tokens = %v", d)
	}
	if u["output_tokens"].(int) != 5 || u["total_tokens"].(int) != 105 {
		t.Errorf("usage = %v", u)
	}
}

// TestResponsesOpenAIToResource 非流式响应翻译：output 条目与 usage。
func TestResponsesOpenAIToResource(t *testing.T) {
	resp := map[string]any{
		"id":      "chatcmpl-1",
		"created": float64(1700000000),
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":              "assistant",
				"content":           "hello",
				"reasoning_content": "thinking hard",
				"tool_calls": []any{map[string]any{
					"id":       "call_9",
					"type":     "function",
					"function": map[string]any{"name": "exec_command", "arguments": `{"cmd":"ls"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(3)},
	}
	got := openAIToResponses(resp, responsesMeta{Model: "deepseek-v4.1-flash"})
	if got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("信封 = %v", got)
	}
	if got["model"] != "deepseek-v4.1-flash" {
		t.Errorf("model = %v", got["model"])
	}
	items, _ := got["output"].([]any)
	if len(items) != 3 {
		t.Fatalf("output 条数 = %d, want 3（reasoning/message/function_call）: %v", len(items), items)
	}
	r0, _ := items[0].(map[string]any)
	if r0["type"] != "reasoning" {
		t.Errorf("items[0] = %v", r0)
	}
	sum, _ := r0["summary"].([]any)
	s0, _ := sum[0].(map[string]any)
	if s0["type"] != "summary_text" || s0["text"] != "thinking hard" {
		t.Errorf("reasoning summary = %v", sum)
	}
	if _, ok := r0["encrypted_content"]; ok {
		t.Errorf("不得编造 encrypted_content: %v", r0)
	}
	m1, _ := items[1].(map[string]any)
	if m1["type"] != "message" || m1["role"] != "assistant" {
		t.Errorf("items[1] = %v", m1)
	}
	c, _ := m1["content"].([]any)
	p0, _ := c[0].(map[string]any)
	if p0["type"] != "output_text" || p0["text"] != "hello" {
		t.Errorf("message content = %v", c)
	}
	if _, ok := p0["annotations"]; !ok {
		t.Errorf("output_text 必须带 annotations: %v", p0)
	}
	f2, _ := items[2].(map[string]any)
	if f2["type"] != "function_call" || f2["call_id"] != "call_9" || f2["name"] != "exec_command" {
		t.Errorf("function_call 条目 = %v", f2)
	}
	u, _ := got["usage"].(map[string]any)
	if u["input_tokens"].(int) != 10 || u["output_tokens"].(int) != 3 {
		t.Errorf("usage = %v", u)
	}
}

// TestResponsesEndpointStream end-to-end：/v1/responses 流式，事件时序 + 收尾。
func TestResponsesEndpointStream(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, responsesSSE(
		`data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"why "},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`,
	), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	order := []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added", `"type":"reasoning"`,
		"event: response.reasoning_summary_text.delta", `"delta":"why "`,
		"event: response.reasoning_summary_text.done",
		"event: response.reasoning_summary_part.done",
		"event: response.output_item.done",
		"event: response.output_item.added", `"type":"message"`,
		"event: response.content_part.added",
		"event: response.output_text.delta", `"delta":"he"`,
		"event: response.output_text.delta", `"delta":"llo"`,
		"event: response.output_text.done", `"text":"hello"`,
		"event: response.content_part.done",
		"event: response.output_item.done",
		`"text":"hello"`, `"status":"completed"`,
		"event: response.completed",
	}
	pos := 0
	for _, want := range order {
		i := strings.Index(body[pos:], want)
		if i < 0 {
			t.Fatalf("事件序列缺少 %q\n--- 实际输出 ---\n%s", want, body)
		}
		pos += i + len(want)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("OpenAI 的 [DONE] 不应出现在 Responses 流里")
	}
	// usage：input_tokens 含缓存（100），cached 单列。
	if !strings.Contains(body, `"input_tokens":100`) || !strings.Contains(body, `"cached_tokens":80`) {
		t.Errorf("completed 应带含缓存的 usage:\n%s", body)
	}
}

// TestResponsesEndpointToolCall 流式工具调用：function_call 条目 + arguments.delta。
func TestResponsesEndpointToolCall(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, responsesSSE(
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":"}}]},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"input":"hi"}`)))
	body := rec.Body.String()
	order := []string{
		`"call_id":"call_7"`, `"name":"exec_command"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done", `"arguments":"{\"cmd\":\"ls\"}"`,
		"event: response.output_item.done", `"status":"completed"`,
		"event: response.completed",
	}
	pos := 0
	for _, want := range order {
		i := strings.Index(body[pos:], want)
		if i < 0 {
			t.Fatalf("工具调用序列缺少 %q\n--- 实际输出 ---\n%s", want, body)
		}
		pos += i + len(want)
	}
}

// TestResponsesEndpointNonStream 非流式：单块 Responses resource（JSON）。
func TestResponsesEndpointNonStream(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, responsesSSE(
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
	), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","input":"hi"}`)))
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %s", rec.Body.String())
	}
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("信封 = %v", out)
	}
	items, _ := out["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output = %v", items)
	}
	m, _ := items[0].(map[string]any)
	c, _ := m["content"].([]any)
	p0, _ := c[0].(map[string]any)
	if p0["text"] != "ok" {
		t.Errorf("正文 = %v", p0)
	}
}

// TestResponsesAuthErrorShape 401 是 OpenAI 错误信封（codex 按 error.message 展示）。
func TestResponsesAuthErrorShape(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, `{}`, false)
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, APIKey: "k1"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`)))
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("401 不是 JSON: %s", rec.Body.String())
	}
	e, _ := out["error"].(map[string]any)
	if e["message"] == "" {
		t.Errorf("401 信封 = %v", out)
	}
}

// TestResponsesEndpointStreamFailedOnUpstreamError 上游流中错误 → response.failed
// （codex 据此报错，而不是把半截流当成正常回答）。
func TestResponsesEndpointStreamFailedOnUpstreamError(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"par"},"finish_reason":null}]}`,
		`data: {"error":{"code":"6004","message":"usage exceeds frequency limit"}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"input":"hi"}`)))
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.failed") || !strings.Contains(body, "6004") {
		t.Errorf("流中错误应终止为 response.failed:\n%s", body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Errorf("出错后不应再发 completed:\n%s", body)
	}
}

// TestResponsesTruncationIsIncomplete 截断必须报 incomplete。
//
// 回归锚：上游 finish_reason=="length"（被 max_tokens 截断）时，Responses 的正确表达是
// status=="incomplete" + incomplete_details.reason=="max_output_tokens"。此前两条路径
// 都硬编码 "completed"，客户端（codex）会把半截正文当最终答案——实测「数到1500」只出到
// 990 却报 completed，正是「输出到一半没了、客户端显示 done」的形态。
func TestResponsesTruncationIsIncomplete(t *testing.T) {
	withChatLog(t)
	// 流式：finish_reason=length。
	up, _ := captureUpstream(t, 200, strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"1,2,3"},"finish_reason":null}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":8000}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"input":"hi"}`)))
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.incomplete") {
		t.Errorf("截断应发 response.incomplete 事件:\n%s", body)
	}
	if !strings.Contains(body, `"status":"incomplete"`) {
		t.Errorf("status 应为 incomplete:\n%s", body)
	}
	if !strings.Contains(body, `"reason":"max_output_tokens"`) {
		t.Errorf("应带 incomplete_details.reason=max_output_tokens:\n%s", body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Errorf("截断不得同时发 completed:\n%s", body)
	}
}

// TestResponsesTruncationNonStream 非流式路径同口径。
func TestResponsesTruncationNonStream(t *testing.T) {
	got := openAIToResponses(map[string]any{
		"id": "c1",
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "1,2,3"},
			"finish_reason": "length",
		}},
		"usage": map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(8000)},
	}, responsesMeta{Model: "m"})
	if got["status"] != "incomplete" {
		t.Errorf("status = %v, want incomplete", got["status"])
	}
	d, _ := got["incomplete_details"].(map[string]any)
	if d["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details = %v", got["incomplete_details"])
	}

	// 正常结束仍为 completed 且 incomplete_details 为 null。
	ok := openAIToResponses(map[string]any{
		"id":      "c1",
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop"}},
	}, responsesMeta{Model: "m"})
	if ok["status"] != "completed" || ok["incomplete_details"] != nil {
		t.Errorf("正常结束应为 completed/null, got status=%v incomplete=%v", ok["status"], ok["incomplete_details"])
	}
}

// TestResponsesUpstreamInterrupted 上游断流必须报 incomplete，不得谎报 completed。
//
// 回归锚：上游连接被掐（只发内容、没有 finish_reason、没有 [DONE]）时，此前 finalize()
// 会补一个 status="completed" + incomplete_details=null —— 客户端完全无法区分「答完了」
// 与「被掐断了」，于是把半截正文当最终答案（用户观感：输出到一半突然没了、却无任何错误）。
// 实测上游正常结束时 finish_reason 与 [DONE] 必然齐备，故缺任一即判定为中断。
func TestResponsesUpstreamInterrupted(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"前半段"},"finish_reason":null}]}`,
		``, // 直接 EOF：无 finish_reason、无 [DONE]
	}, "\n\n"), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"input":"hi"}`)))
	body := rec.Body.String()
	// 断言要覆盖 response 级与 item 级两处 status（都不得是 completed）。
	if strings.Contains(body, `"status":"completed"`) {
		t.Errorf("上游断流不得报 completed（客户端会把半截内容当完整答案）:\n%s", body)
	}
	if !strings.Contains(body, `"status":"incomplete"`) {
		t.Errorf("response 与 item 都应报 incomplete:\n%s", body)
	}
	if !strings.Contains(body, "event: response.incomplete") {
		t.Errorf("上游断流应发 response.incomplete:\n%s", body)
	}
	if !strings.Contains(body, `"reason":"upstream_interrupted"`) {
		t.Errorf("应带 incomplete_details.reason=upstream_interrupted:\n%s", body)
	}
	// 已产出的半截正文仍要交出去（incomplete 语义是「没写完」而非「丢弃」）。
	if !strings.Contains(body, "前半段") {
		t.Errorf("断流前的内容应保留:\n%s", body)
	}
}

// TestResponsesNormalFinishStillCompleted 正常收尾（finish_reason + [DONE] 齐备）仍是 completed。
func TestResponsesNormalFinishStillCompleted(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, responsesSSE(
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
	), true) // responsesSSE 自带 finish_reason=stop 帧 + [DONE]
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"input":"hi"}`)))
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, `"status":"completed"`) {
		t.Errorf("正常收尾应为 completed:\n%s", body)
	}
	if strings.Contains(body, "response.incomplete") {
		t.Errorf("正常收尾不应出现 incomplete:\n%s", body)
	}
}

// TestResponsesNonStreamUpstreamInterrupted 非流式：上游断流必须报 incomplete。
//
// 与流式路径同源：Aggregate 无 finish_reason 时置断流标记，非流式翻译必须读到它并
// 报 incomplete（而非 completed —— 后者让客户端把半截正文当最终答案）。
func TestResponsesNonStreamUpstreamInterrupted(t *testing.T) {
	withChatLog(t)
	up, _ := captureUpstream(t, 200, strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"半截"},"finish_reason":null}]}`,
		``,
	}, "\n\n"), true)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","input":"hi"}`)))
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"incomplete"`) {
		t.Errorf("非流式上游断流应报 incomplete:\n%s", body)
	}
	if !strings.Contains(body, `"reason":"upstream_interrupted"`) {
		t.Errorf("应带 incomplete_details.reason=upstream_interrupted:\n%s", body)
	}
	if strings.Contains(body, upstream.TruncatedKey) {
		t.Errorf("内部截断标记不得出现在响应里:\n%s", body)
	}
	// item 级 status 必须与 response 级同口径：否则客户端按 item 判定仍以为写完了。
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是 JSON: %s", body)
	}
	items, _ := out["output"].([]any)
	if len(items) == 0 {
		t.Fatalf("应有 output 条目: %s", body)
	}
	for i, it := range items {
		m, _ := it.(map[string]any)
		if m["status"] != "incomplete" {
			t.Errorf("item[%d].status = %v, want incomplete（与 response 级同口径）", i, m["status"])
		}
	}
}
