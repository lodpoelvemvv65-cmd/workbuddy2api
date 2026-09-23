// responses.go OpenAI Responses API（POST /v1/responses）兼容层。
//
// 设计：与 anthropic.go 同源的**协议垫片**，不另起一套上游调用管线。
//
//	Responses 请求 --翻译--> OpenAI chat/completions body
//	                      --复用 chatCompletions-->（账号池 / 会话粘性 /
//	  prompt_cache_key / 指纹清洗 / 错误分类 / 成本账本 / metrics 全部原样继承）
//	                      --翻译--> Responses 响应（SSE 事件序列 或 单块 JSON）
//
// 服务对象是 codex-cli（wire_api = "responses"）：它的请求形态实测如下
// （`codex-cli 0.155.1`，抓包自 POST /v1/responses）：
//
//	{"model","instructions","input":[...],"tools":[...],"tool_choice":"auto",
//	 "parallel_tool_calls":true,"reasoning":{"summary":"auto"},"store":false,
//	 "stream":true,"include":["reasoning.encrypted_content"],
//	 "prompt_cache_key":"<会话 uuid>","client_metadata":{...}}
//
// # 缓存契约（本层最关键、最不能丢的约束）
//
// 好消息：codex 直接带 prompt_cache_key（= 会话 uuid），于是三条链路全部自带：
// session.ExtractKey 的兜底优先级 5 认它（会话粘性）、upstream.InjectPromptCacheKey
// 的优先级 1 原值保留它（上游缓存键按会话稳定）。本层**不做任何改写**，把该字段原样
// 透传——多一层映射只会把客户端已经正确的会话标识弄坏。
//
// # 与 Anthropic 层的形态差异（翻译时必须显式处理）
//
//   - input 是**条目数组**（message / function_call / function_call_output / reasoning），
//     不是单一 messages 数组：function_call 要并成一条 assistant.tool_calls，
//     function_call_output 要变成独立 role:"tool" 消息（与 Anthropic 的 tool_result
//     同构）；reasoning 条目直接丢弃（上游不认该形态，且 encrypted_content 只在
//     官方侧成立）。
//   - 工具定义是**扁平**的（name/parameters 与 type 同级），而 Anthropic 是嵌套
//     input_schema；namespace / web_search / custom 等上游无对应物的工具类型整条丢弃
//     （宁可不提供，也不能把上游不认的字段直传导致整请求 400）。
//   - 响应侧 usage 口径与 Anthropic **相反**：input_tokens 是含缓存的总额，命中缓存的
//     部分单列 input_tokens_details.cached_tokens（Anthropic 是剔除法）。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// ── 请求翻译 ──────────────────────────────────────────────────────────────

// responsesMeta 翻译产物中 handler 侧需要复用的元信息。
type responsesMeta struct {
	// Model 客户端原始模型名，回显在 Responses 响应里。
	Model string
	// Stream 客户端是否要求流式（codex 恒 true）。
	Stream bool
	// InputEstimate 出站 body 的粗略输入 token 估算，供流首帧 usage.input_tokens。
	InputEstimate int
}

// responsesRequest Responses 请求体（只声明本层需要翻译的字段）。
type responsesRequest struct {
	Model              string              `json:"model"`
	Instructions       json.RawMessage     `json:"instructions"`
	Input              json.RawMessage     `json:"input"`
	MaxOutputTokens    int                 `json:"max_output_tokens"`
	Stream             bool                `json:"stream"`
	Tools              []responsesTool     `json:"tools"`
	ToolChoice         json.RawMessage     `json:"tool_choice"`
	Temperature        *float64            `json:"temperature"`
	TopP               *float64            `json:"top_p"`
	Reasoning          *responsesReasoning `json:"reasoning"`
	Text               json.RawMessage     `json:"text"`
	PromptCacheKey     string              `json:"prompt_cache_key"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls"`
	Metadata           map[string]any      `json:"metadata"`
	PreviousResponseID string              `json:"previous_response_id"`
}

type responsesReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

// responsesTool 工具定义。Responses 的 function 工具把 name/parameters 摊平在同级
// （不像 chat/completions 再套一层 function）；namespace 类型自带嵌套 tools。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
	Tools       []responsesTool `json:"tools"` // type=="namespace" 的嵌套工具
}

// responsesInputItem input 数组元素（各 type 的字段并存，按 Type 取用）。
type responsesInputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// function_call_output
	Output json.RawMessage `json:"output"`
	// reasoning
	Summary json.RawMessage `json:"summary"`
}

// responsesPart 内容块（input_text / output_text / text / input_image）。
type responsesPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
}

// errInvalidResponsesBody 请求体解析失败（不可重试的客户端错误）。
var errInvalidResponsesBody = errString("invalid request body: not a JSON object")

// responsesToOpenAI 把 Responses 请求体翻译成 OpenAI chat/completions body。
func responsesToOpenAI(body []byte, defaultModel string) ([]byte, responsesMeta, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, responsesMeta{}, errInvalidResponsesBody
	}
	meta := responsesMeta{Model: req.Model, Stream: req.Stream}

	out := map[string]any{}
	model := req.Model
	if strings.TrimSpace(model) == "" {
		model = defaultModel // 客户端没写模型 → 落网关默认模型（上游对空模型只会给无信息错误）
	}
	if model != "" {
		out["model"] = model
	}

	msgs := []any{}
	if s := responsesText(req.Instructions); s != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": s})
	}
	msgs = append(msgs, responsesInputToMessages(req.Input)...)
	out["messages"] = msgs

	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = req.MaxOutputTokens
	}
	if req.Stream {
		out["stream"] = true
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if tools := responsesTools(req.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := responsesToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if rf := responsesResponseFormat(req.Text); rf != nil {
		out["response_format"] = rf
	}
	applyResponsesReasoning(out, req.Reasoning)
	// 缓存契约：客户端自带 prompt_cache_key 时**原样透传**（绝不改写）。
	// codex 的取值是会话 uuid，一条字段同时喂饱会话粘性（session.ExtractKey 的
	// prompt_cache_key 兜底）与上游缓存键（upstream.InjectPromptCacheKey 优先级 1
	// 客户端自带即保留），网关再做一层映射只会把已经正确的标识弄坏。
	if k := strings.TrimSpace(req.PromptCacheKey); k != "" {
		out["prompt_cache_key"] = k
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, meta, err
	}
	meta.InputEstimate = estimateBodyTokens(out)
	return raw, meta, nil
}

// responsesInputToMessages 把 input（字符串 / 条目对象 / 条目数组）翻成 chat messages。
//
// 连续的同一次 assistant 工具调用簇（function_call…）合并进一条 assistant 消息的
// tool_calls 数组——这是 chat 协议的规范形态，也是并行工具调用的唯一正确表达
// （codex 的 parallel_tool_calls 默认 true，会连发多个 function_call 再连发结果）。
func responsesInputToMessages(raw json.RawMessage) []any {
	out := []any{}
	if len(raw) == 0 {
		return out
	}
	// 字符串形态：等价于一条 user 消息。
	if s := responsesText(raw); s != "" {
		return append(out, map[string]any{"role": "user", "content": s})
	}
	items := []responsesInputItem{}
	if json.Unmarshal(raw, &items) != nil {
		var one responsesInputItem
		if json.Unmarshal(raw, &one) != nil {
			return out
		}
		items = append(items, one)
	}
	for _, it := range items {
		switch it.Type {
		case "function_call":
			msg := map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id":   responsesCallID(it.CallID),
					"type": "function",
					"function": map[string]any{
						"name":      it.Name,
						"arguments": responsesArguments(it.Arguments),
					},
				}},
			}
			// 与上一条 assistant 工具消息并簇（并行工具调用）。
			if n := len(out); n > 0 {
				if prev, ok := out[n-1].(map[string]any); ok && prev["tool_calls"] != nil && prev["content"] == nil {
					if tcs, ok := prev["tool_calls"].([]any); ok {
						prev["tool_calls"] = append(tcs, msg["tool_calls"].([]any)...)
						continue
					}
				}
			}
			out = append(out, msg)
		case "function_call_output":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": responsesCallID(it.CallID),
				"content":      responsesToolOutput(it.Output),
			})
		case "reasoning":
			// 丢弃：上游不认 reasoning 条目，且带 signature/encrypted_content 的形态
			// 只在官方侧能校验。codex 下一轮会把它带回来，这里静默吃掉即可。
		default:
			// message（含 role 的条目）与未声明 type 的形态：按角色消息处理。
			role := it.Role
			if role == "" {
				continue
			}
			content := responsesContent(it.Content)
			if content == nil {
				continue
			}
			out = append(out, map[string]any{"role": responsesRole(role), "content": content})
		}
	}
	return out
}

// responsesRole Responses 角色 → chat 角色。developer 是 OpenAI 新系的系统级角色，
// 上游只认 system（直传 developer 会被判非法 role）。
func responsesRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "developer", "system":
		return "system"
	case "assistant":
		return "assistant"
	case "tool":
		return "tool"
	default:
		return "user"
	}
}

// responsesContent content 字段 → chat 的 content（字符串或 parts 数组）。
// 纯文本收敛成字符串（省 token、避免上游对 parts 的兼容差异）；含图片才用数组。
func responsesContent(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	if s := responsesText(raw); s != "" {
		return s
	}
	var parts []responsesPart
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	texts := []string{}
	rich := []any{}
	for _, p := range parts {
		switch p.Type {
		case "input_image", "image_url":
			if p.ImageURL == "" {
				continue
			}
			img := map[string]any{"url": p.ImageURL}
			if p.Detail != "" {
				img["detail"] = p.Detail
			}
			rich = append(rich, map[string]any{"type": "image_url", "image_url": img})
		default:
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
	}
	if len(rich) == 0 {
		if len(texts) == 0 {
			return nil
		}
		return strings.Join(texts, "\n")
	}
	if len(texts) > 0 {
		rich = append([]any{map[string]any{"type": "text", "text": strings.Join(texts, "\n")}}, rich...)
	}
	return rich
}

// responsesText 取「字符串 / {text}/ [{text}]」三种形态里的纯文本（无则空串）。
func responsesText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var one struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &one) == nil && one.Text != "" {
		return one.Text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		buf := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Text != "" {
				buf = append(buf, p.Text)
			}
		}
		return strings.Join(buf, "\n")
	}
	return ""
}

// responsesToolOutput function_call_output.output → 字符串（对象则原样 JSON 化）。
func responsesToolOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// responsesCallID 工具调用 id 兜底（codex 的 call_id 一定非空；自研客户端可能省）。
func responsesCallID(id string) string {
	if strings.TrimSpace(id) == "" {
		return "call_" + session.NewMessageID()
	}
	return id
}

// responsesArguments arguments 兜底：chat 协议要求它是 JSON 字符串。
func responsesArguments(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

// responsesTools Responses 工具定义 → chat tools。
//
// 只翻译 type=="function"（含 strict）。namespace / web_search / custom / local_shell
// 等类型上游 chat 协议没有对应物，整条丢弃：上游对未知工具类型是直接 400 拒整请求，
// 直传的代价是「整个对话不可用」，丢弃的代价只是「少一个工具」。
func responsesTools(tools []responsesTool) []any {
	out := []any{}
	for _, t := range tools {
		if t.Type != "function" || t.Name == "" {
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.Parameters) > 0 {
			fn["parameters"] = json.RawMessage(t.Parameters)
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if t.Strict != nil {
			fn["strict"] = *t.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// responsesToolChoice tool_choice → chat 形态；无有效映射时返回 nil（不带该字段）。
func responsesToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none", "required":
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	if obj.Type == "function" && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	// allowed_tools / shell / web_search… 上游无对应：退到 auto（不锁死工具选择）。
	if obj.Type == "allowed_tools" {
		return "auto"
	}
	return nil
}

// responsesResponseFormat text.format → chat response_format（结构化输出）。
// 只认 json_schema / json_object；文本形态（默认）不产生该字段。
func responsesResponseFormat(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var t struct {
		Format json.RawMessage `json:"format"`
	}
	if json.Unmarshal(raw, &t) != nil || len(t.Format) == 0 {
		return nil
	}
	var f struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict"`
	}
	if json.Unmarshal(t.Format, &f) != nil {
		return nil
	}
	switch f.Type {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		js := map[string]any{"name": f.Name, "schema": json.RawMessage(f.Schema)}
		if f.Name == "" {
			js["name"] = "response"
		}
		if f.Strict != nil {
			js["strict"] = *f.Strict
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	}
	return nil
}

// applyResponsesReasoning reasoning.effort → 出站 thinking + reasoning_effort。
//
// codex 只要 effort 非空就代表「用户要求思考」（配置 model_reasoning_effort）；
// effort 为空（codex 只发 {"summary":"auto"} 的常见形态）则一律不动出站字段——
// 凭 summary 的存在就开思考会让每一次请求都变慢，而 summary 只描述「要不要回传
// 思考摘要」，不是「要不要思考」。
func applyResponsesReasoning(out map[string]any, r *responsesReasoning) {
	if r == nil {
		return
	}
	e := responsesEffort(r.Effort)
	if e == "" {
		return
	}
	out["thinking"] = map[string]any{"type": "enabled"}
	out["reasoning_effort"] = e
}

// responsesEffort Responses 档位 → 上游档位（上游认 low/medium/high）。
func responsesEffort(e string) string {
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh":
		return "high"
	}
	return ""
}

// ── handler ──────────────────────────────────────────────────────────────

// responses POST /v1/responses：OpenAI Responses 协议入口（codex-cli 的 wire_api）。
//
// 流程与 messages 完全同构：读 body → 翻译成 chat body → 复用 chatCompletions →
// 转换型 ResponseWriter 把写出的 OpenAI 响应翻回 Responses 形态。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	out, meta, err := responsesToOpenAI(body, h.cfg.AnthropicDefaultModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(out))
	r2.ContentLength = int64(len(out))
	r2.Header.Set("Content-Type", "application/json")
	rw := newResponsesWriter(w, meta)
	h.chatCompletions(rw, r2)
	rw.finish()
}

// ── 响应翻译：共用小工具 ──────────────────────────────────────────────────

// responsesResponseID 生成响应 id。
func responsesResponseID() string { return "resp_" + session.NewMessageID() }

// responsesItemID 生成条目 id（前缀区分 reasoning / message / function_call）。
func responsesItemID(prefix string) string { return prefix + "_" + session.NewMessageID() }

// responsesUsage 上游 usage → Responses usage。
//
// 口径与 Anthropic **相反**（别照抄 anthropicUsage）：Responses 的 input_tokens 是
// **含缓存**的总额，命中部分单列 input_tokens_details.cached_tokens。故这里不做减法，
// 只把总额搬过来 + 附明细。
func responsesUsage(u map[string]any, estimate int) map[string]any {
	prompt := intField(u, "prompt_tokens")
	completion := intField(u, "completion_tokens")
	cached := maxInt(intField(u, "cache_read_input_tokens"), intField(u, "prompt_cache_hit_tokens"))
	reasoning := intField(u, "reasoning_tokens")
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		reasoning = maxInt(reasoning, intField(d, "reasoning_tokens"))
	}
	if prompt == 0 && completion == 0 {
		prompt = estimate // 上游没给 usage（异常流）：保留估算值，好过全 0。
	}
	return map[string]any{
		"input_tokens":          prompt,
		"output_tokens":         completion,
		"total_tokens":          prompt + completion,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
	}
}

// responsesFunctionCallItem 由 chat 的 tool_calls 元素构造 function_call 条目。
func responsesFunctionCallItem(tc map[string]any, status string) map[string]any {
	fn, _ := tc["function"].(map[string]any)
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	id := responsesCallID(strAny(tc["id"]))
	return map[string]any{
		"type":      "function_call",
		"id":        "fc_" + strings.TrimPrefix(id, "call_"),
		"call_id":   id,
		"name":      name,
		"arguments": responsesArguments(args),
		"status":    status,
	}
}

// responsesReasoningItem 由 chat 的 reasoning_content 构造 reasoning 条目。
//
// 只带 summary（codex 把它当思考摘要显示），**不带 encrypted_content**：网关拿不到
// 官方签名，编造一个只会让上游在下一轮校验失败。实测 codex 0.155 接受无加密内容的
// reasoning 条目并正常回传。
func responsesReasoningItem(id, text string) map[string]any {
	return map[string]any{
		"type":    "reasoning",
		"id":      id,
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
}

// strAny 取 map 里的字符串值（非 string 或缺失返回 ""）。
func strAny(v any) string {
	s, _ := v.(string)
	return s
}

// ── 响应翻译：非流式 ──────────────────────────────────────────────────────

// openAIToResponses 把聚合后的 OpenAI chat.completion 翻译成 Responses resource。
func openAIToResponses(resp map[string]any, meta responsesMeta) map[string]any {
	id, _ := resp["id"].(string)
	if id == "" {
		id = responsesResponseID()
	}
	created := intField(resp, "created")
	if created == 0 {
		created = int(time.Now().Unix())
	}
	model := meta.Model
	if model == "" {
		model = strAny(resp["model"])
	}
	// 截断事实必须在构造条目**之前**取走：下面的 itemStatusOf 要用它决定条目终态
	// （TakeTruncated 会删除标记键，取晚了就拿不到了）。finishReason=="length"
	// 的情形由 finishReasonOf 单独覆盖，两者等价于同一个「没写完」事实。
	truncated := upstream.TakeTruncated(resp)
	itemStatus := itemStatusOf(truncated, finishReasonOf(resp))

	out := []any{}
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if msg, ok := c["message"].(map[string]any); ok {
				if rc := strAny(msg["reasoning_content"]); rc != "" {
					out = append(out, responsesReasoningItem("rs_"+session.NewMessageID(), rc))
				}
				if text := responsesTextOf(msg["content"]); text != "" {
					out = append(out, map[string]any{
						"type":   "message",
						"id":     "msg_" + session.NewMessageID(),
						"status": itemStatus,
						"role":   "assistant",
						"content": []any{map[string]any{
							"type": "output_text", "text": text, "annotations": []any{},
						}},
					})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, t := range tcs {
						if tm, ok := t.(map[string]any); ok {
							out = append(out, responsesFunctionCallItem(tm, itemStatus))
						}
					}
				}
			}
		}
	}
	usage := map[string]any{}
	if u, ok := resp["usage"].(map[string]any); ok {
		usage = responsesUsage(u, meta.InputEstimate)
	} else {
		usage = responsesUsage(map[string]any{}, meta.InputEstimate)
	}
	// 截断语义与流式路径同口径（见 respStream.finish / failInterrupted 注释）：
	//   - finish_reason=="length"：被 max_tokens 截断；
	//   - TruncatedKey：上游连接被掐（EOF 收尾且没给 finish_reason）。
	// 两者都必须报 incomplete 而非 completed，否则客户端把半截正文当最终答案。
	// TakeTruncated 顺带把内部标记键取走，保证它不出现在对外响应里。
	status, incomplete := "completed", any(nil)
	switch {
	case finishReasonOf(resp) == "length":
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	case truncated:
		status = "incomplete"
		incomplete = map[string]any{"reason": "upstream_interrupted"}
	}
	return map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          created,
		"status":              status,
		"model":               model,
		"output":              out,
		"usage":               usage,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"error":               nil,
		"incomplete_details":  incomplete,
	}
}

// itemStatusOf 非流式路径的条目终态（与 respStream.itemStatus 同口径）。
//
// 流式路径已按截断/中断报 incomplete，非流式此前硬编码 "completed"——于是同一份
// 半截正文，流式说 incomplete、非流式说 completed，客户端按 item 判定就会误以为
// 这条消息写完了（response 级 status 对了也没用）。
func itemStatusOf(truncated bool, fr string) string {
	if truncated || fr == "length" {
		return "incomplete"
	}
	return "completed"
}

// finishReasonOf 取聚合响应的 finish_reason（缺省 "stop"）。
func finishReasonOf(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return "stop"
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return "stop"
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		return fr
	}
	return "stop"
}

// responsesTextOf chat message.content（字符串或 parts 数组）→ 纯文本。
func responsesTextOf(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		buf := []string{}
		for _, p := range v {
			if pm, ok := p.(map[string]any); ok {
				if t := strAny(pm["text"]); t != "" {
					buf = append(buf, t)
				}
			}
		}
		return strings.Join(buf, "")
	}
	return ""
}

// ── 响应翻译：流式 ────────────────────────────────────────────────────────

// respStream 把 OpenAI SSE 帧序列翻译成 Responses SSE 事件序列。
//
// 事件时序（codex 逐帧解析，缺 response.completed 直接判流中断并重连）：
//
//	response.created → response.in_progress
//	→ (reasoning: output_item.added → reasoning_summary_part.added →
//	   reasoning_summary_text.delta* → …_text.done → …_part.done → output_item.done)*
//	→ (message: output_item.added → content_part.added →
//	   output_text.delta* → output_text.done → content_part.done → output_item.done)*
//	→ (function_call: output_item.added → function_call_arguments.delta* →
//	   function_call_arguments.done → output_item.done)*
//	→ response.completed
//
// 与 Anthropic 状态机的对应关系：output_index ≈ content block index，item.done ≈
// content_block_stop，且必须**关旧项才能开新项**（codex 按 index 归并，交叉开项会串行）。
type respStream struct {
	w  io.Writer
	fl http.Flusher

	meta      responsesMeta
	respID    string
	model     string
	createdAt int64
	seq       int

	started bool
	done    bool
	errored bool

	outputs []any // 已产出条目（response.completed 里原样回填）

	reasonID   string
	reasonIdx  int
	reasonOpen bool
	reasonBuf  strings.Builder

	msgID   string
	msgIdx  int
	msgOpen bool
	textBuf strings.Builder

	toolState map[int]*respToolState

	usage map[string]any

	// finishReason 上游给的结束原因（"stop"/"length"/"tool_calls"…）。
	// 必须参与收尾判定：OpenAI 用 finish_reason=="length" 表达「被 max_tokens 截断」，
	// 而 Responses 用 status=="incomplete" + incomplete_details.reason=="max_output_tokens"
	// 表达同一件事。若不看它，被截断的流会被报成 status="completed"——客户端据此
	// 认为回答已完整（codex 会直接把半截正文当最终答案，不再续写、不报错），
	// 这正是「输出到一半没了、显示 done」的形态。
	finishReason string

	// sawDone 上游是否显式发过 data: [DONE]。
	// 与 finishReason 一起构成「流是否正常收尾」的判据：实测上游正常结束时**必然**
	// 两者齐备（finish_reason 帧 + [DONE]，10/10）；缺任一个即上游中断（连接被掐、
	// 上游 5xx 中途断流）。此时若照旧补一个 status="completed"，客户端会把半截正文
	// 当成完整答案——既不知道被截断、也不会重试，这正是「输出到一半没了」的形态。
	sawDone bool

	// interrupted 上游在收尾前断流（无 finish_reason）。条目 status 据此报 incomplete，
	// 与 response 级 status 保持同口径（否则 response=incomplete 而 item=completed，
	// 严格客户端按 item 判定仍会以为这条消息写完了）。
	interrupted bool
}

// itemStatus 条目终态：被截断/中断时 "incomplete"，否则 "completed"。
func (s *respStream) itemStatus() string {
	if s.finishReason == "length" || s.interrupted {
		return "incomplete"
	}
	return "completed"
}

// respToolState 单个 chat tool_calls.index 对应的 function_call 条目状态。
type respToolState struct {
	itemIdx int
	id      string
	callID  string
	name    string
	args    strings.Builder
	opened  bool
}

func newRespStream(w io.Writer, fl http.Flusher, meta responsesMeta) *respStream {
	return &respStream{
		w:         w,
		fl:        fl,
		meta:      meta,
		respID:    responsesResponseID(),
		model:     meta.Model,
		createdAt: time.Now().Unix(),
		toolState: map[int]*respToolState{},
	}
}

// emit 写一个 Responses SSE 事件（event: 行 + data: 行 + 空行），带自增 sequence_number。
func (s *respStream) emit(event string, payload map[string]any) {
	if _, ok := payload["sequence_number"]; !ok {
		payload["sequence_number"] = s.seq
	}
	s.seq++
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = io.WriteString(s.w, "event: "+event+"\n")
	_, _ = io.WriteString(s.w, "data: "+string(raw)+"\n\n")
	if s.fl != nil {
		s.fl.Flush()
	}
}

// envelope 当前响应对象的快照（status 由调用方决定）。
func (s *respStream) envelope(status string) map[string]any {
	model := s.model
	return map[string]any{
		"id":                  s.respID,
		"object":              "response",
		"created_at":          s.createdAt,
		"status":              status,
		"model":               model,
		"output":              append([]any{}, s.outputs...),
		"usage":               nil,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"error":               nil,
		"incomplete_details":  nil,
	}
}

// start 惰性发出 created + in_progress（首个内容帧或收尾时各触发一次）。
func (s *respStream) start() {
	if s.started {
		return
	}
	s.started = true
	env := s.envelope("in_progress")
	s.emit("response.created", map[string]any{"type": "response.created", "response": env})
	s.emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": env})
}

// closeAll 关闭所有仍打开的条目（切类型、收尾都要先关干净）。
func (s *respStream) closeAll() {
	s.closeReasoning()
	s.closeMessage()
	for _, st := range s.toolState {
		if st.opened {
			s.closeTool(st)
		}
	}
}

// ── reasoning 条目 ──

func (s *respStream) openReasoning() {
	if s.reasonOpen {
		return
	}
	s.closeMessage()
	s.closeTools()
	s.start()
	s.reasonID = responsesItemID("rs")
	s.reasonIdx = len(s.outputs)
	s.reasonOpen = true
	s.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": s.reasonIdx,
		"item": map[string]any{"type": "reasoning", "id": s.reasonID, "summary": []any{}},
	})
	s.emit("response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "item_id": s.reasonID,
		"output_index": s.reasonIdx, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
}

func (s *respStream) closeReasoning() {
	if !s.reasonOpen {
		return
	}
	s.reasonOpen = false
	item := responsesReasoningItem(s.reasonID, s.reasonBuf.String())
	s.outputs = append(s.outputs, item)
	s.emit("response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "item_id": s.reasonID,
		"output_index": s.reasonIdx, "summary_index": 0, "text": s.reasonBuf.String(),
	})
	s.emit("response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "item_id": s.reasonID,
		"output_index": s.reasonIdx, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": s.reasonBuf.String()},
	})
	s.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": s.reasonIdx, "item": item,
	})
}

// ── message 条目 ──

func (s *respStream) openMessage() {
	if s.msgOpen {
		return
	}
	s.closeReasoning()
	s.closeTools()
	s.start()
	s.msgID = responsesItemID("msg")
	s.msgIdx = len(s.outputs)
	s.msgOpen = true
	s.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": s.msgIdx,
		"item": map[string]any{
			"type": "message", "id": s.msgID, "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	})
	s.emit("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": s.msgID,
		"output_index": s.msgIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (s *respStream) closeMessage() {
	if !s.msgOpen {
		return
	}
	s.msgOpen = false
	text := s.textBuf.String()
	item := map[string]any{
		"type": "message", "id": s.msgID, "status": s.itemStatus(), "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	s.outputs = append(s.outputs, item)
	s.emit("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": s.msgID,
		"output_index": s.msgIdx, "content_index": 0, "text": text,
	})
	s.emit("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": s.msgID,
		"output_index": s.msgIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	s.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": s.msgIdx, "item": item,
	})
}

// ── function_call 条目 ──

// openTool 按 chat tool_calls.index 打开/复用 function_call 条目。
// name/call_id 取首个非空值（StreamHint 会把非首片的 name 收敛掉，故必须缓存）。
func (s *respStream) openTool(idx int, callID, name string) *respToolState {
	st := s.toolState[idx]
	if st == nil {
		st = &respToolState{itemIdx: -1}
		s.toolState[idx] = st
	}
	if callID != "" {
		st.callID = callID
	}
	if name != "" {
		st.name = name
	}
	if !st.opened {
		s.closeReasoning()
		s.closeMessage()
		s.start()
		st.callID = responsesCallID(st.callID)
		st.itemIdx = len(s.outputs)
		st.opened = true
		s.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": st.itemIdx,
			"item": map[string]any{
				"type": "function_call", "id": "fc_" + strings.TrimPrefix(st.callID, "call_"),
				"call_id": st.callID, "name": st.name, "arguments": "", "status": "in_progress",
			},
		})
	}
	return st
}

// closeTool 收尾一个 function_call 条目（发 arguments.done + item.done）。
func (s *respStream) closeTool(st *respToolState) {
	if !st.opened {
		return
	}
	st.opened = false
	args := responsesArguments(st.args.String())
	item := map[string]any{
		"type": "function_call", "id": "fc_" + strings.TrimPrefix(st.callID, "call_"),
		"call_id": st.callID, "name": st.name, "arguments": args, "status": s.itemStatus(),
	}
	s.outputs = append(s.outputs, item)
	s.emit("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": "fc_" + strings.TrimPrefix(st.callID, "call_"),
		"output_index": st.itemIdx, "arguments": args,
	})
	s.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": st.itemIdx, "item": item,
	})
}

// closeTools 关闭全部仍打开的 function_call 条目（按 index 序遍历，保序）。
func (s *respStream) closeTools() {
	for i := 0; i < len(s.toolState); i++ {
		if st := s.toolState[i]; st != nil {
			s.closeTool(st)
		}
	}
}

// ── 帧处理 ──

// frame 处理一个上游 SSE 帧 payload（已剥去 "data: " 前缀）。
func (s *respStream) frame(payload string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return // 非 JSON 帧（心跳等）丢弃
	}
	if e, ok := obj["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		if msg == "" {
			msg = strings.TrimSpace(payload)
		}
		s.start()
		s.errored = true
		s.emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": s.respID, "object": "response", "created_at": s.createdAt,
				"status": "failed", "model": s.model, "output": []any{},
				"error": map[string]any{"code": strAny(e["code"]), "message": msg},
			},
		})
		return
	}
	if s.model == "" {
		if m := strAny(obj["model"]); m != "" {
			s.model = m
		}
	}
	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		s.usage = u
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return
	}
	if d, ok := c["delta"].(map[string]any); ok {
		if rc := strAny(d["reasoning_content"]); rc != "" {
			s.openReasoning()
			s.reasonBuf.WriteString(rc)
			s.emit("response.reasoning_summary_text.delta", map[string]any{
				"type": "response.reasoning_summary_text.delta", "item_id": s.reasonID,
				"output_index": s.reasonIdx, "summary_index": 0, "delta": rc,
			})
		}
		if txt := strAny(d["content"]); txt != "" {
			s.openMessage()
			s.textBuf.WriteString(txt)
			s.emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": s.msgID,
				"output_index": s.msgIdx, "content_index": 0, "delta": txt,
			})
		}
		if tcs, ok := d["tool_calls"].([]any); ok {
			s.toolCalls(tcs)
		}
		if fc, ok := d["function_call"].(map[string]any); ok {
			s.legacyFunctionCall(fc)
		}
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		s.finish(fr)
	}
}

// toolCalls 处理 delta.tool_calls 分片。
func (s *respStream) toolCalls(tcs []any) {
	for _, t := range tcs {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tm["function"].(map[string]any)
		st := s.openTool(intField(tm, "index"), strAny(tm["id"]), strAny(fn["name"]))
		if args := strAny(fn["arguments"]); args != "" {
			st.args.WriteString(args)
			s.emit("response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      "fc_" + strings.TrimPrefix(st.callID, "call_"),
				"output_index": st.itemIdx, "delta": args,
			})
		}
	}
}

// legacyFunctionCall 处理旧式 delta.function_call（无 index/id，视为单工具）。
func (s *respStream) legacyFunctionCall(fc map[string]any) {
	st := s.openTool(0, "", strAny(fc["name"]))
	if args := strAny(fc["arguments"]); args != "" {
		st.args.WriteString(args)
		s.emit("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_" + strings.TrimPrefix(st.callID, "call_"),
			"output_index": st.itemIdx, "delta": args,
		})
	}
}

// finish 收到 finish_reason：关条目 → response.completed / response.incomplete（带真实 usage）。
//
// 截断（finish_reason=="length"）走 Responses 的 incomplete 语义：status="incomplete"
// + incomplete_details.reason="max_output_tokens"。客户端（codex 等）据此知道回答被
// 截断（官方 SDK 会把该 response 判为未完成），而不是把半截正文当最终答案。
func (s *respStream) finish(fr string) {
	if s.done || s.errored {
		return
	}
	s.finishReason = fr
	status, incomplete := "completed", any(nil)
	if fr == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	s.start()
	s.closeAll()
	env := s.envelope(status)
	env["incomplete_details"] = incomplete
	if s.usage != nil {
		env["usage"] = responsesUsage(s.usage, s.meta.InputEstimate)
	} else {
		env["usage"] = responsesUsage(map[string]any{}, s.meta.InputEstimate)
	}
	event := "response.completed"
	if status != "completed" {
		event = "response.incomplete"
	}
	s.emit(event, map[string]any{"type": event, "response": env})
	s.done = true
}

// finalize 流的收尾兜底：上游没给 finish_reason 就断流（或断在 [DONE] 之前）时，
// 必须让客户端能区分「答完了」与「被掐断了」。
//
// 判据：finishReason 非空 **且** sawDone 为真才算正常收尾（实测上游正常结束必然两者
// 齐备）。否则报 status="incomplete" + incomplete_details.reason="upstream_interrupted"。
// 为什么不照旧补 completed：客户端（codex 等）只认 status，completed 意味着「这是最终
// 答案」——半截正文会被当作完整回答采用，用户看到的就是「输出到一半突然没了、却没有
// 任何错误」。宁可显式声明未完成（客户端会据此重试/提示），也不能谎报完成。
//
// 已出错/已收尾则空操作。
func (s *respStream) finalize() {
	if s.errored || s.done {
		return
	}
	if s.finishReason != "" && s.sawDone {
		s.finish(s.finishReason)
		return
	}
	// 拿到 finish_reason 但没等到 [DONE]：内容本身完整（finish_reason 已声明结束原因），
	// 只是收尾帧缺失。按 finish_reason 的语义收尾（含 length → incomplete）。
	if s.finishReason != "" {
		s.finish(s.finishReason)
		return
	}
	s.failInterrupted()
}

// failInterrupted 上游中断收尾：不是 error 帧（上游没报错），而是流被截断。
// 用 incomplete + upstream_interrupted 表达，与 response.failed（真错误）区分开：
// failed 会让客户端当错误处理并重试整轮，incomplete 只表示「这次没写完」。
func (s *respStream) failInterrupted() {
	s.interrupted = true
	noteUpstreamTruncation("responses", s.meta.Model, true)
	s.start()
	s.closeAll()
	env := s.envelope("incomplete")
	env["incomplete_details"] = map[string]any{"reason": "upstream_interrupted"}
	if s.usage != nil {
		env["usage"] = responsesUsage(s.usage, s.meta.InputEstimate)
	} else {
		env["usage"] = responsesUsage(map[string]any{}, s.meta.InputEstimate)
	}
	s.emit("response.incomplete", map[string]any{"type": "response.incomplete", "response": env})
	s.done = true
}

// ── 转换型 ResponseWriter ─────────────────────────────────────────────────

const (
	respModeUnknown = iota
	respModeSSE
	respModeJSON
)

// responsesWriter 包住真实 ResponseWriter，把 chatCompletions 写出的 OpenAI 响应
// 翻译成 Responses 形态（状态码/响应头仍由 chatCompletions 决定）。
type responsesWriter struct {
	w  http.ResponseWriter
	fl http.Flusher

	meta   responsesMeta
	buf    bytes.Buffer
	mode   int
	stream *respStream
	status int
}

func newResponsesWriter(w http.ResponseWriter, meta responsesMeta) *responsesWriter {
	fl, _ := w.(http.Flusher)
	return &responsesWriter{w: w, fl: fl, meta: meta, status: http.StatusOK}
}

func (r *responsesWriter) Header() http.Header { return r.w.Header() }

// consumesUpstreamTruncation 标记本层会自行读取上游断流标记（见 handler.truncationAware）。
func (r *responsesWriter) consumesUpstreamTruncation() {}

func (r *responsesWriter) WriteHeader(status int) {
	// 只记录、不转发（同 anthropicWriter：非流式要等 body 完整才能翻译，
	// 转发会让收尾的 writeJSON/writeOpenAIError 二次 WriteHeader）。
	r.status = status
}

func (r *responsesWriter) Flush() {
	if r.fl != nil {
		r.fl.Flush()
	}
}

func (r *responsesWriter) Write(p []byte) (int, error) {
	if r.mode == respModeUnknown {
		r.detect(p)
	}
	r.buf.Write(p)
	if r.mode == respModeSSE {
		r.drain()
	}
	return len(p), nil
}

// detect 按首块内容判定写出模式（SSE 帧恒以 "data: " 或心跳 ":" 开头，JSON 恒 "{"）。
func (r *responsesWriter) detect(p []byte) {
	s := strings.TrimLeft(string(p), " \t\r\n")
	switch {
	case strings.HasPrefix(s, "data:"), strings.HasPrefix(s, ":"):
		r.mode = respModeSSE
		r.stream = newRespStream(r.w, r.fl, r.meta)
	default:
		r.mode = respModeJSON
	}
}

// drain 把缓冲里已完整的 SSE 帧（以空行分隔）逐帧翻译。
func (r *responsesWriter) drain() {
	for {
		raw := r.buf.Bytes()
		idx := bytes.Index(raw, []byte("\n\n"))
		if idx < 0 {
			return
		}
		frame := string(raw[:idx])
		r.buf.Next(idx + 2)
		r.handleFrame(frame)
	}
}

// handleFrame 从一帧里取出所有 data: 行并交给翻译器。
func (r *responsesWriter) handleFrame(frame string) {
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue // 心跳/注释行
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			// 上游显式收尾标记：finalize 正常收尾判据的一半（见 respStream.sawDone）。
			r.stream.sawDone = true
			continue
		}
		r.stream.frame(payload)
	}
}

// finish 由 handler 在 chatCompletions 返回后调用，完成收尾翻译。
func (r *responsesWriter) finish() {
	switch r.mode {
	case respModeSSE:
		r.drain()
		r.stream.finalize()
	case respModeJSON:
		r.finishJSON()
	default:
		writeOpenAIError(r.w, http.StatusInternalServerError, "empty_upstream_response", "empty upstream response")
	}
}

// finishJSON 把缓冲的 OpenAI JSON 响应翻译成 Responses 形态（错误信封或 response）。
func (r *responsesWriter) finishJSON() {
	raw := r.buf.Bytes()
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		writeOpenAIError(r.w, r.status, "invalid_upstream_response", strings.TrimSpace(string(raw)))
		return
	}
	if e, ok := obj["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		code, _ := e["code"].(string)
		if msg == "" {
			msg = "upstream error"
		}
		writeOpenAIError(r.w, r.status, code, msg)
		return
	}
	// 非流式上游断流（Aggregate 置 TruncatedKey）：留一行 WARN + 计数。
	// 只 peek 不取走——标记由 openAIToResponses 统一消费并译成 incomplete。
	if _, truncated := obj[upstream.TruncatedKey]; truncated {
		noteUpstreamTruncation("responses", r.meta.Model, false)
	}
	writeJSON(r.w, r.status, openAIToResponses(obj, r.meta))
}
