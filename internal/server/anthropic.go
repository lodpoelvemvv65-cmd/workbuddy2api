// anthropic.go Anthropic Messages API（POST /v1/messages）兼容层。
//
// 设计：**协议垫片**，不另起一套上游调用管线。
//
//	Anthropic 请求 --翻译--> OpenAI chat/completions body
//	                       --复用 chatCompletions-->（账号池 / 会话粘性 /
//	  prompt_cache_key 注入 / 指纹清洗 / 错误分类 / 成本账本 / metrics 全部原样继承）
//	                       --翻译--> Anthropic 响应（SSE 或 JSON）
//
// 之所以复用而不是重写：chatCompletions 里 540 行的轮转/冷却/降级状态机是网关的
// 核心资产，任何"再写一份上游调用"的路线都会让这层兼容接口在可靠性上沦为二等公民。
//
// # 缓存契约（本层最关键、最不能丢的约束）
//
// Anthropic 客户端（Claude Code / 官方 SDK）的请求体里**没有** OpenAI 系的
// conversation_id，会话标识只藏在 metadata.user_id（形如
// user_<hash>_account_<uuid>_session_<uuid>）。若不做映射，会同时踩两个坑：
//
//  1. session.ExtractKey 恒返回空 → 会话粘性失效 → 同一会话在账号池里逐请求轮换
//     换号（同一段前缀被拆到不同账号，缓存全废）；
//  2. upstream.InjectPromptCacheKey 的 convHex 退化成定值 → 每账号一个固定前缀键，
//     多会话互相顶掉彼此的缓存。
//
// 故本层把 metadata.user_id 映射进出站 body 的 conversation_id。一处注入同时喂饱
// 三条链路：粘性（ExtractKey）、上游会话头族（ResolveConversationID）、
// 缓存键（InjectPromptCacheKey 优先级 2）。实测同前缀二连击：hit 1792/2036、
// credit 0.18 → 0.03（≈6×），与 OpenAI 原生客户端完全同口径。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// ── 请求翻译 ──────────────────────────────────────────────────────────────

// /v1/messages 里 claude* 模型名的内置映射目标（CN / global 各一）。
//
// 选型：两域都用 deepseek-v4.1-flash，context_length 1M（/v1/models 实测透出
// 1000000）、max_output 128K、倍率低。可用 config anthropic.default_model /
// anthropic.default_model_global 覆盖。
const (
	defaultAnthropicModel       = "deepseek-v4.1-flash"
	defaultAnthropicModelGlobal = "global:deepseek-v4.1-flash"
)

// anthroMeta 翻译产物中handler 侧需要复用的元信息。
type anthroMeta struct {
	// Model 客户端原始模型名，回显在 Anthropic 响应里（客户端看到的仍是它要的模型）。
	Model string
	// Stream 客户端是否要求流式。
	Stream bool
	// ConversationID 从 metadata 派生的会话标识（空 = 派生不出，退化为轮级键）。
	ConversationID string
	// InputEstimate 出站 body 的粗略输入 token 估算，供 message_start.usage.input_tokens。
	// 上游 usage 只在末帧给出，而该字段在首帧就要写；给估算值好过恒 0（客户端据此
	// 做上下文占用/自动压缩判断，恒 0 会让它以为上下文是空的）。
	InputEstimate int
}

// anthroRequest Anthropic Messages 请求体（只声明本层需要的字段）。
type anthroRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	System      json.RawMessage `json:"system"`
	Messages    []anthroMessage `json:"messages"`
	Tools       []anthroTool    `json:"tools"`
	ToolChoice  json.RawMessage `json:"tool_choice"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	StopSeqs    []string        `json:"stop_sequences"`
	Metadata    map[string]any  `json:"metadata"`
	Thinking    json.RawMessage `json:"thinking"`
}

type anthroMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthroTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthroBlock 内容块（Anthropic 的 content 数组元素，字段按 type 二选一）。
type anthroBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// image
	Source *anthroSource `json:"source"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type anthroSource struct {
	Type      string `json:"type"` // base64 / url
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// anthropicToOpenAI 把 Anthropic Messages 请求体翻译成 OpenAI chat/completions body。
//
// 翻译规则（对齐官方语义，不含猜测性改写）：
//   - system（字符串或 text 块数组）→ 首条 {"role":"system"}；
//   - user/assistant 字符串 content → 原样；块数组 → text/image_url parts；
//     tool_use → assistant.tool_calls；tool_result → 独立 {"role":"tool"} 消息
//     （OpenAI 的 tool 结果必须是独立消息，Anthropic 内嵌在 user 消息里）；
//   - thinking / redacted_thinking 块**丢弃**：上游不认该形态，且带 signature 回流
//     校验只在 Anthropic 侧成立，透传只会被拒；
//   - tools[{name,description,input_schema}] → OpenAI tools[{type:function,...}]；
//   - tool_choice：auto/any/tool 三态映射；
//   - stop_sequences → stop；thinking → 出站 thinking:{type:...} + reasoning_effort。
func anthropicToOpenAI(body []byte, defaultModel string) ([]byte, anthroMeta, error) {
	var req anthroRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, anthroMeta{}, errInvalidAnthropicBody
	}
	meta := anthroMeta{
		Model:          req.Model,
		Stream:         req.Stream,
		ConversationID: anthropicConversationID(req.Metadata),
	}

	out := map[string]any{}
	// claude* 模型名映射到网关默认模型：Claude Code 默认发 claude-sonnet-* 之类，
	// 上游没有这些名字，直传必 404。显式配了 ANTHROPIC_MODEL 的客户端不受影响。
	if m := resolveAnthropicModel(req.Model, defaultModel); m != "" {
		out["model"] = m
	}
	msgs := []any{}
	if sys := anthropicSystemText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, anthropicMessageToOpenAI(m)...)
	}
	out["messages"] = msgs
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
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
	if len(req.StopSeqs) > 0 {
		out["stop"] = req.StopSeqs
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				fn["parameters"] = json.RawMessage(t.InputSchema)
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}
	if tc := anthropicToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	applyAnthropicThinking(out, req.Thinking, req.Model)
	// 会话标识：粘性 + 上游会话头族 + prompt_cache_key 三处共用（见文件头缓存契约）。
	if meta.ConversationID != "" {
		out["conversation_id"] = meta.ConversationID
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, meta, err
	}
	meta.InputEstimate = estimateBodyTokens(out)
	return raw, meta, nil
}

// errInvalidAnthropicBody 请求体解析失败（不可重试的客户端错误）。
var errInvalidAnthropicBody = errString("invalid request body: not a JSON object")

type errString string

func (e errString) Error() string { return string(e) }

// anthropicConversationID 从 Anthropic metadata 派生会话标识。
//
// 优先 user_id（Claude Code 等官方客户端的形态，同会话内恒定）；其次 metadata 里
// 的 conversation_id（非标但无害，部分自研客户端会带上）。两者皆无 → 空串，
// 此时 handler 侧仍能靠 StickyFallbackKey（首条 user 消息）+ TurnKey 拾回会话/轮级
// 粒度，缓存不至于完全失效，只是不如显式标识精确。
func anthropicConversationID(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	for _, k := range []string{"user_id", "conversation_id"} {
		if v, ok := meta[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// contextMarkerRe 模型名尾部的上下文窗口标记（Claude Code 系写法 [1m]/[200k]/[1000000]）。
var contextMarkerRe = regexp.MustCompile(`(?i)^(.+)\[(\d+)([km])?\]$`)

// stripContextMarker 剥掉模型名尾部的上下文标记。
//
// 为什么必须在网关侧剥：上游实测不认该后缀——"deepseek-v4-flash[1m]" 直传得到
// 503 no such model（见测试注释）。而 [1m] 正是 Claude Code 表达「按 1M 上下文跑」
// 的写法，故网关收下这个意图、把干净的模型名发给上游。
func stripContextMarker(model string) string {
	// 单一事实来源：与候选链的档位解析 parseContextMarker 共用同一条正则与剥除规则
	// （标记永远不是模型名的一部分，无论档位值是否合法都剥）。正则要求标记前至少一个
	// 字符，故 "[1m]" 这类无模型名的畸形输入原样返回，不会被剥成空串。
	bare, _ := parseContextMarker(model)
	return bare
}

// resolveAnthropicModel 决定出站模型名。三条规则，按优先级：
//
//  1. 透传优先：客户端写什么模型就用什么（含 realm 前缀），网关不替客户端选模型。
//  2. 剥离上下文标记：模型名尾部的 [1m] / [200k] / [1000000] 剥掉。上游实测不认该
//     后缀（"deepseek-v4-flash[1m]" 直传 -> 503 no such model）；而 [1m] 正是 Claude
//     Code 表达「按 1M 上下文跑」的写法。目标模型本身就带 1M（/v1/models 实测
//     deepseek-v4.1-flash = 1000000），故剥掉后缀即等于「有 1M 就用 1M」。
//  3. claude* 兜底：claude-sonnet-* 这类是客户端（Claude Code）的内置默认名，上游
//     并不存在，直传必失败 -> 落配置的默认模型（config anthropic.default_model，
//     内置 deepseek-v4.1-flash，CN 1M）。客户端只要显式配了网关模型名，就走规则 1。
//
// realm 前缀（resolveModel 协议）先剥后拼，两处都需要它：
//
//   - 透传路径：客户端写 "global:deepseek-v4.1-flash[1m]" 时必须重组回带前缀的净名，
//     否则前缀被吃掉、请求错落到 CN 域；
//   - 兜底路径：写 "global:claude-sonnet-4-5" 时要给默认模型补上 global 前缀——
//     国际版与 CN 的模型池不同，这正是「国际版也要这个」的落点。配置项自身带
//     "global:" 前缀时（常见于纯国际版部署）以配置为准，不再叠加。
func resolveAnthropicModel(model, defaultModel string) string {
	if model == "" {
		return ""
	}
	realm, bare := resolveModel(model)
	bare = stripContextMarker(bare)
	if bare == "" {
		return ""
	}
	if !isClaudeName(bare) {
		// 规则 1：透传，保留客户端给的 realm 归属。
		if realm == "global" {
			return "global:" + bare
		}
		return bare
	}
	// 规则 3：claude* 上游不存在 -> 落默认模型。
	target := stripContextMarker(defaultModel)
	if target == "" {
		target = defaultAnthropicModel
	}
	dRealm, dBare := resolveModel(target)
	if dRealm == "global" {
		// 配置显式指定 global 域目标 -> 以配置为准。
		return "global:" + dBare
	}
	if realm == "global" {
		return "global:" + dBare
	}
	return dBare
}

// isClaudeName 判定是否为 Anthropic 系模型名（claude-sonnet-4-5 等）。
func isClaudeName(bare string) bool {
	return strings.HasPrefix(strings.ToLower(bare), "claude")
}

// anthropicSystemText 取 system 字段文本：字符串形态 / 块数组形态（只取 text 块）。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthroBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// anthropicMessageToOpenAI 翻译单条消息；可能展开成多条（tool_result 独立成
// role=tool 消息），故返回切片。
func anthropicMessageToOpenAI(m anthroMessage) []any {
	role := m.Role
	if role != "assistant" {
		role = "user"
	}
	// content 是字符串：直接透传。
	var s string
	if len(m.Content) > 0 && json.Unmarshal(m.Content, &s) == nil {
		return []any{map[string]any{"role": role, "content": s}}
	}
	var blocks []anthroBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		// 畸形 content：退回空串，避免把坏结构喂给上游（与 prepareBody 同哲学）。
		return []any{map[string]any{"role": role, "content": ""}}
	}

	outs := []any{}
	parts := []any{}     // 多模态 parts（text / image_url）
	toolCalls := []any{} // assistant 的 tool_use
	text := strings.Builder{}
	// hasMedia 标记本消息是否出现了非文本 part（图片）。它决定收尾时用哪种
	// content 形态：字符串装不下 image_url，含图必须走 parts 数组。
	hasMedia := false

	flushParts := func() {
		if len(parts) == 0 {
			return
		}
		outs = append(outs, map[string]any{"role": role, "content": parts})
		parts = []any{}
		// text 与 parts 同源累积，flush 掉 parts 就必须同时清 text，
		// 否则收尾分支会把同一段文本再发一条（消息重复）。
		text.Reset()
	}
	flushToolCalls := func() {
		if len(toolCalls) == 0 {
			return
		}
		msg := map[string]any{"role": "assistant", "tool_calls": toolCalls}
		if text.Len() > 0 {
			msg["content"] = text.String()
			text.Reset()
		}
		outs = append(outs, msg)
		toolCalls = []any{}
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case "image":
			if u := anthropicImageURL(b.Source); u != "" {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": u},
				})
				hasMedia = true
			}
		case "tool_use":
			// OpenAI 的 tool_calls 与 content 挂在**同一条** assistant 消息上，
			// 故此处不能把文本单独 flush 成消息（会多出一条），只清 parts，
			// 文本交给 flushToolCalls 一并带出。
			parts = nil
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		case "tool_result":
			flushParts()
			flushToolCalls()
			outs = append(outs, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      anthropicToolResultText(b.Content),
			})
		case "thinking", "redacted_thinking":
			// 丢弃：见函数注释。
		}
	}
	flushToolCalls()
	switch {
	case hasMedia:
		// 含图：必须 parts 形态（含此前的 text part）。
		flushParts()
	case text.Len() > 0:
		// 纯文本走字符串形态（与 OpenAI 客户端同口径，少一层 parts 包装）。
		outs = append(outs, map[string]any{"role": role, "content": text.String()})
		text.Reset()
		parts = nil
	default:
		flushParts()
	}
	if len(outs) == 0 {
		outs = append(outs, map[string]any{"role": role, "content": ""})
	}
	return outs
}

// anthropicImageURL 把 Anthropic image source 转成 OpenAI 的 image_url。
func anthropicImageURL(src *anthroSource) string {
	if src == nil {
		return ""
	}
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return ""
		}
		mt := src.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + src.Data
	case "url":
		return src.URL
	}
	return ""
}

// anthropicToolResultText 取 tool_result 的文本（字符串或块数组，图片块丢弃）。
func anthropicToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthroBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// anthropicToolChoice 映射 tool_choice：auto→"auto"、any→"required"、
// tool(named)→function 指定；缺失或畸形返回 nil（不带字段，上游自行决定）。
func anthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	switch v.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if v.Name == "" {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": v.Name}}
	}
	return nil
}

// applyAnthropicThinking 把 Anthropic thinking 开关映射到出站字段。
//
// 上游（WorkBuddy）对 deepseek 系认 thinking:{type:"enabled"}（见 upstream/thinking.go
// 的逆向结论），Anthropic 的 {type:"enabled",budget_tokens:N} 语义可直译为
// 出站 thinking + reasoning_effort 档位：预算越大档越高。
// disabled → 出站 thinking:{type:"disabled"}（thinking.go 会照抄该 case 删 effort）。
func applyAnthropicThinking(out map[string]any, raw json.RawMessage, model string) {
	if len(raw) == 0 {
		return
	}
	var v struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Type == "" {
		return
	}
	switch v.Type {
	case "enabled":
		out["thinking"] = map[string]any{"type": "enabled"}
		out["reasoning_effort"] = effortForBudget(v.BudgetTokens)
	case "disabled":
		out["thinking"] = map[string]any{"type": "disabled"}
	}
}

// effortForBudget budget_tokens → reasoning_effort 档位（上游按模型支持度降级）。
func effortForBudget(budget int) string {
	switch {
	case budget <= 0:
		return "medium"
	case budget <= 4096:
		return "low"
	case budget <= 16384:
		return "medium"
	default:
		return "high"
	}
}

// ── token 估算 ────────────────────────────────────────────────────────────

// estimateTokens 粗略 token 估算：ASCII 约 4 字符/token，非 ASCII（CJK 等）约 1 字符/token。
// 仅用于 message_start.usage 与 count_tokens 的预估值——真实数字由上游末帧 usage 覆盖。
func estimateTokens(s string) int {
	ascii, non := 0, 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			non++
		}
	}
	return (ascii+3)/4 + non + 1
}

// estimateBodyTokens 估算出站请求体的输入 token 量（serialize 长度法，够用即止）。
func estimateBodyTokens(body map[string]any) int {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0
	}
	return estimateTokens(string(raw))
}

// ── handler ──────────────────────────────────────────────────────────────

// messages POST /v1/messages：Anthropic Messages 协议入口。
//
// 流程：读 Anthropic body → 翻译成 OpenAI body → 交给 chatCompletions（整条既有
// 管线）→ 用一个转换型 ResponseWriter 把写出的 OpenAI 响应翻译回 Anthropic 形态。
// 这样账号池/粘性/缓存键/清洗/错误策略/账本全部原样生效，本层只负责协议形状。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	out, meta, err := anthropicToOpenAI(body, h.cfg.AnthropicDefaultModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(out))
	r2.ContentLength = int64(len(out))
	r2.Header.Set("Content-Type", "application/json")
	aw := newAnthropicWriter(w, meta)
	h.chatCompletions(aw, r2)
	aw.finish()
}

// messagesCountTokens POST /v1/messages/count_tokens：Claude Code 等客户端会调用它
// 做上下文预算。网关侧没有分词器，用与 message_start 同源的估算口径（见
// estimateBodyTokens）给出预估值——该端点只影响客户端的自我调度，不参与计费。
func (h *Handler) messagesCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	out, _, err := anthropicToOpenAI(body, h.cfg.AnthropicDefaultModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	var obj map[string]any
	if json.Unmarshal(out, &obj) != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateBodyTokens(obj)})
}

// ── 错误 ─────────────────────────────────────────────────────────────────

// writeAnthropicError 以 Anthropic 错误信封回写（type 按 HTTP 状态推断）。
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	writeAnthropicErrorCode(w, status, "", msg)
}

// writeAnthropicErrorCode 同上，另在 error 对象上带 code（上游业务码原样透传，
// 便于排查；Anthropic SDK 读 type/message，多余键被忽略）。
func writeAnthropicErrorCode(w http.ResponseWriter, status int, code, msg string) {
	e := map[string]any{"type": anthropicErrType(status), "message": msg}
	if code != "" {
		e["code"] = code
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": e})
}

// anthropicErrType HTTP 状态 → Anthropic error.type（对齐官方枚举）。
func anthropicErrType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// ── 响应翻译：非流式 ──────────────────────────────────────────────────────

// openAIToAnthropicMessage 把聚合后的 OpenAI chat.completion 翻译成 Anthropic message。
func openAIToAnthropicMessage(resp map[string]any, meta anthroMeta) map[string]any {
	id, _ := resp["id"].(string)
	if id == "" {
		id = "msg_" + session.NewMessageID()
	}
	model := meta.Model
	if model == "" {
		if m, ok := resp["model"].(string); ok {
			model = m
		}
	}
	stopReason := "end_turn"
	blocks := []any{}
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if msg, ok := c["message"].(map[string]any); ok {
				if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
					blocks = append(blocks, map[string]any{"type": "thinking", "thinking": rc, "signature": ""})
				}
				blocks = append(blocks, textBlocksFromContent(msg["content"])...)
				blocks = append(blocks, toolUseBlocksFromToolCalls(msg["tool_calls"])...)
			}
			if fr, ok := c["finish_reason"].(string); ok {
				stopReason = anthropicStopReason(fr)
			}
		}
	}
	usage := map[string]any{"input_tokens": meta.InputEstimate, "output_tokens": 0}
	if u, ok := resp["usage"].(map[string]any); ok {
		usage = anthropicUsage(u, meta.InputEstimate)
	}
	return map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	}
}

// textBlocksFromContent 把 OpenAI 的 content（字符串或 parts 数组）翻成 text 块。
func textBlocksFromContent(content any) []any {
	out := []any{}
	switch v := content.(type) {
	case string:
		if v != "" {
			out = append(out, map[string]any{"type": "text", "text": v})
		}
	case []any:
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok && t != "" {
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		}
	}
	return out
}

// toolUseBlocksFromToolCalls 把 OpenAI tool_calls 翻成 Anthropic tool_use 块。
// arguments 是 JSON 字符串；解析失败时原样塞进 {"_raw": "..."}（不丢内容）。
func toolUseBlocksFromToolCalls(tc any) []any {
	out := []any{}
	list, ok := tc.([]any)
	if !ok {
		return out
	}
	for _, t := range list {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tm["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		var input any = map[string]any{}
		if strings.TrimSpace(args) != "" {
			if json.Unmarshal([]byte(args), &input) != nil {
				input = map[string]any{"_raw": args}
			}
		}
		id, _ := tm["id"].(string)
		if id == "" {
			id = "toolu_" + session.NewMessageID()
		}
		out = append(out, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
	}
	return out
}

// anthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// anthropicUsage 上游 usage → Anthropic usage。
//
// 口径差异（必须显式换算，否则客户端上下文统计偏大）：Anthropic 的 input_tokens
// **不含**命中缓存的部分，缓存前缀单独计入 cache_read_input_tokens；而上游的
// prompt_tokens 是含缓存的总额（实测 1792 命中 + 244 未命中 = 2036 prompt）。
// 故 input_tokens = prompt_tokens - cache_read（钳零）。
func anthropicUsage(u map[string]any, estimate int) map[string]any {
	prompt := intField(u, "prompt_tokens")
	completion := intField(u, "completion_tokens")
	read := maxInt(intField(u, "cache_read_input_tokens"), intField(u, "prompt_cache_hit_tokens"))
	written := maxInt(intField(u, "cache_creation_input_tokens"), intField(u, "prompt_cache_write_tokens"))
	in := prompt - read
	if in < 0 {
		in = 0
	}
	if prompt == 0 && completion == 0 {
		// 上游没给 usage（异常流）：保留估算值，好过全 0。
		in = estimate
	}
	return map[string]any{
		"input_tokens":                in,
		"output_tokens":               completion,
		"cache_read_input_tokens":     read,
		"cache_creation_input_tokens": written,
	}
}

func intField(m map[string]any, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ── 响应翻译：流式 ────────────────────────────────────────────────────────

// anthroStream 把 OpenAI SSE 帧序列翻译成 Anthropic SSE 事件序列。
//
// Anthropic 事件时序（严格顺序，客户端按 index 归并 content block）：
//
//	message_start → (content_block_start → content_block_delta* → content_block_stop)*
//	              → message_delta → message_stop
//
// 与 OpenAI 的形态差异有三处需要状态机承接：
//   - content block 有开闭（OpenAI 只有 delta 流）；
//   - 思考链是独立的 thinking 块（OpenAI 塞在 delta.reasoning_content）；
//   - 工具调用是 tool_use 块 + input_json_delta 分片（OpenAI 是 tool_calls 分片）。
type anthroStream struct {
	w  io.Writer
	fl http.Flusher

	meta      anthroMeta
	msgID     string
	started   bool
	nextIdx   int
	openIdx   int
	openKind  string
	toolState map[int]*anthroToolState
	usage     map[string]any
	errored   bool
	stopped   bool

	// finishReason 上游显式给出的结束原因（"stop"/"length"/"tool_calls"…）。
	// 这是本层判断「流是否完整」的**唯一**可靠信号：下游看到的 [DONE] 恒由
	// upstream.StreamHint 兜底补齐（无论上游是否发过，见 sse.go 的收尾段），
	// 故 [DONE] 的有无在这里不携带任何信息，不能用它当收尾判据。
	finishReason string

	// interrupted 上游在收尾前被掐断（自始至终没给 finish_reason）。
	// 由 failInterrupted 置位，保证收尾路径与内容路径对截断的判定同口径。
	interrupted bool
}

// anthroToolState 单个 OpenAI tool_calls.index 对应的 Anthropic 块状态。
type anthroToolState struct {
	blockIdx int
	id       string
	name     string
	opened   bool
}

func newAnthroStream(w io.Writer, fl http.Flusher, meta anthroMeta) *anthroStream {
	return &anthroStream{
		w:         w,
		fl:        fl,
		meta:      meta,
		openIdx:   -1,
		toolState: map[int]*anthroToolState{},
	}
}

// emit 写一个 Anthropic SSE 事件（event: 行 + data: 行 + 空行）。
// 官方每个事件都同时带 event 与 data.type；两者都发，SDK 与手写解析器都能用。
func (s *anthroStream) emit(event string, payload map[string]any) {
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

// start 惰性发出 message_start（首个内容帧到达或收尾时各触发一次）。
func (s *anthroStream) start() {
	if s.started {
		return
	}
	s.started = true
	if s.msgID == "" {
		s.msgID = "msg_" + session.NewMessageID()
	}
	s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.meta.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			// input_tokens 只能是估算：上游 usage 在末帧才给，而本帧必须最先发。
			"usage": map[string]any{"input_tokens": s.meta.InputEstimate, "output_tokens": 0},
		},
	})
}

// closeBlock 关闭当前打开的 content block（无则空操作）。
func (s *anthroStream) closeBlock() {
	if s.openIdx < 0 {
		return
	}
	s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.openIdx})
	s.openIdx, s.openKind = -1, ""
}

// openBlock 打开/复用同类型块，返回其 index。切换内容类型时先关旧块。
func (s *anthroStream) openBlock(kind string) int {
	if s.openIdx >= 0 && s.openKind == kind {
		return s.openIdx
	}
	s.closeBlock()
	s.start()
	idx := s.nextIdx
	s.nextIdx++
	cb := map[string]any{"type": kind}
	switch kind {
	case "text":
		cb["text"] = ""
	case "thinking":
		cb["thinking"] = ""
	}
	s.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx, "content_block": cb,
	})
	s.openIdx, s.openKind = idx, kind
	return idx
}

// openToolBlock 按 OpenAI tool_calls.index 打开/复用 tool_use 块。
// id/name 取首个非空值（StreamHint 会把非首片的 name 收敛掉，故必须缓存）。
func (s *anthroStream) openToolBlock(toolIdx int, id, name string) *anthroToolState {
	st := s.toolState[toolIdx]
	if st == nil {
		st = &anthroToolState{blockIdx: -1}
		s.toolState[toolIdx] = st
	}
	if id != "" {
		st.id = id
	}
	if name != "" {
		st.name = name
	}
	if !st.opened {
		s.closeBlock()
		s.start()
		if st.id == "" {
			st.id = "toolu_" + session.NewMessageID()
		}
		st.blockIdx = s.nextIdx
		s.nextIdx++
		s.emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": st.blockIdx,
			"content_block": map[string]any{
				"type": "tool_use", "id": st.id, "name": st.name, "input": map[string]any{},
			},
		})
		st.opened = true
		// 打开后即成为"当前块"，后续 input_json_delta 直接按 blockIdx 发。
		s.openIdx, s.openKind = st.blockIdx, "tool_use"
	}
	return st
}

// delta 发一个 content_block_delta。
func (s *anthroStream) delta(idx int, d map[string]any) {
	s.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": idx, "delta": d,
	})
}

// frame 处理一个上游 SSE 帧 payload（已剥去 "data: " 前缀）。
func (s *anthroStream) frame(payload string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return // 非 JSON 帧（心跳等）丢弃
	}
	if e, ok := obj["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		code, _ := e["code"].(string)
		if msg == "" {
			msg = strings.TrimSpace(payload)
		}
		// 流中错误：Anthropic 用 error 事件终止，客户端按异常抛出。
		s.start()
		errObj := map[string]any{"type": "api_error", "message": msg}
		if code != "" {
			errObj["code"] = code
		}
		s.emit("error", map[string]any{"type": "error", "error": errObj})
		s.errored = true
		return
	}
	if id, ok := obj["id"].(string); ok && id != "" && s.msgID == "" {
		s.msgID = id
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
		// 思考链 → thinking 块。
		if rc, ok := d["reasoning_content"].(string); ok && rc != "" {
			idx := s.openBlock("thinking")
			s.delta(idx, map[string]any{"type": "thinking_delta", "thinking": rc})
		}
		// 正文 → text 块。
		if txt, ok := d["content"].(string); ok && txt != "" {
			idx := s.openBlock("text")
			s.delta(idx, map[string]any{"type": "text_delta", "text": txt})
		}
		// 工具调用 → tool_use 块 + input_json_delta。
		if tcs, ok := d["tool_calls"].([]any); ok {
			s.toolCalls(tcs)
		}
		if fc, ok := d["function_call"].(map[string]any); ok {
			s.legacyFunctionCall(fc)
		}
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		s.finishReason = fr
		s.finish(fr)
	}
}

// toolCalls 处理 delta.tool_calls 分片。
func (s *anthroStream) toolCalls(tcs []any) {
	for _, t := range tcs {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		toolIdx := intField(tm, "index")
		id, _ := tm["id"].(string)
		fn, _ := tm["function"].(map[string]any)
		name, _ := fn["name"].(string)
		st := s.openToolBlock(toolIdx, id, name)
		if args, ok := fn["arguments"].(string); ok && args != "" {
			s.delta(st.blockIdx, map[string]any{"type": "input_json_delta", "partial_json": args})
		}
	}
}

// legacyFunctionCall 处理旧式 delta.function_call（无 index/id，视为单工具）。
func (s *anthroStream) legacyFunctionCall(fc map[string]any) {
	name, _ := fc["name"].(string)
	st := s.openToolBlock(0, "", name)
	if args, ok := fc["arguments"].(string); ok && args != "" {
		s.delta(st.blockIdx, map[string]any{"type": "input_json_delta", "partial_json": args})
	}
}

// finish 收到 finish_reason：收块 → message_delta（含真实 usage）→ message_stop。
func (s *anthroStream) finish(fr string) {
	if s.stopped || s.errored {
		return
	}
	s.start()
	s.closeBlock()
	usage := anthropicUsage(s.usage, s.meta.InputEstimate)
	s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(fr), "stop_sequence": nil},
		// 真实数字（上游末帧 usage）：input 与缓存字段一并带上，供非 SDK 解析器
		// 取到准确值（SDK 只覆盖 output_tokens，input 仍用 message_start 的估算）。
		"usage": usage,
	})
	s.emit("message_stop", map[string]any{"type": "message_stop"})
	s.stopped = true
}

// finalize 流的收尾兜底。判据是 finishReason：拿到了就按它收尾（含 length →
// max_tokens，客户端据此提示"超出输出上限"），始终没拿到就是被掐断了。
//
// 为什么不能照旧补 stop_reason:"end_turn"：Anthropic 的 end_turn 语义是「模型
// 自然说完」，客户端据此把已收到的正文当**最终答案**采纳——半截正文会被静默吞下，
// 既不重试也不报错。这正是「输出到一半突然没了、却显示正常结束」的成因（与
// Responses 层修 status=completed 的那处同源）。已出错/已收尾则空操作。
func (s *anthroStream) finalize() {
	if s.errored || s.stopped {
		return
	}
	if s.finishReason != "" {
		s.finish(s.finishReason)
		return
	}
	s.failInterrupted()
}

// failInterrupted 上游在收尾前断流（未给 finish_reason）时的收尾。
//
// 这里不能复用 Responses 层的 incomplete 语义——Anthropic 协议里没有 incomplete
// 概念，stop_reason 枚举只有 end_turn / max_tokens / stop_sequence / tool_use /
// pause_turn / refusal，没有任何取值能表达「这次没写完」：end_turn 是谎报答完，
// pause_turn 的语义是「模型暂停、可续跑」（会被当成合法终态静默接受）。
//
// 官方的流中失败形态是 **error 事件**：anthropic-sdk 读到 event: error 即抛异常
// （Claude Code 据此走重试/报错路径，而不是把半截正文当答案）。故此处按该形态
// 收尾——先关掉未闭合的内容块，再发 error，让客户端拿到结构完整且语义诚实的流尾。
func (s *anthroStream) failInterrupted() {
	s.interrupted = true
	noteUpstreamTruncation("messages", s.meta.Model, true)
	s.start()
	s.closeBlock()
	s.emit("error", map[string]any{"type": "error", "error": map[string]any{
		"type":    "api_error",
		"message": "upstream stream ended before completion (no finish_reason)",
	}})
	s.errored = true
}

// ── 转换型 ResponseWriter ─────────────────────────────────────────────────

// 写出模式：首字节决定这是 SSE 流还是单块 JSON。判定依据是 StreamHint 的帧恒以
// "data: "（或心跳 ":"）开头，而 writeJSON 恒以 "{" 开头——两者无歧义。
const (
	anthroModeUnknown = iota
	anthroModeSSE
	anthroModeJSON
)

// anthropicWriter 包住真实 ResponseWriter，把 chatCompletions 写出的 OpenAI
// 响应翻译成 Anthropic 形态。
//
//   - SSE 模式：逐帧翻译并即时 flush（保住 TTFB 与流式体验，不整段缓冲）；
//   - JSON 模式：整块缓冲，等 handler 返回后一次性翻译（非流式响应本身很小，
//     且需要在 Body 完整后再决定 stop_reason/usage）。
//
// 状态码与响应头全权交给 chatCompletions 决定（Content-Type 两边都是
// text/event-stream / application/json，无需改写）。
type anthropicWriter struct {
	w  http.ResponseWriter
	fl http.Flusher

	meta   anthroMeta
	buf    bytes.Buffer
	mode   int
	stream *anthroStream
	status int
}

func newAnthropicWriter(w http.ResponseWriter, meta anthroMeta) *anthropicWriter {
	fl, _ := w.(http.Flusher)
	return &anthropicWriter{w: w, fl: fl, meta: meta, status: http.StatusOK}
}

func (a *anthropicWriter) Header() http.Header { return a.w.Header() }

// consumesUpstreamTruncation 标记本层会自行读取上游断流标记（见 handler.truncationAware）。
func (a *anthropicWriter) consumesUpstreamTruncation() {}

func (a *anthropicWriter) WriteHeader(status int) {
	// 只记录、不转发：JSON 模式要等 body 完整后才能翻译，转发会让收尾的
	// writeJSON/writeAnthropicError 二次 WriteHeader（net/http 报
	// "superfluous response.WriteHeader"，每个非流式请求刷一行噪声日志）。
	// SSE 模式则无需转发——Content-Type 已由 upstream.StreamHint 设好，
	// 首次写入自动带上 200。
	a.status = status
}

func (a *anthropicWriter) Flush() {
	if a.fl != nil {
		a.fl.Flush()
	}
}

func (a *anthropicWriter) Write(p []byte) (int, error) {
	if a.mode == anthroModeUnknown {
		a.detect(p)
	}
	a.buf.Write(p)
	if a.mode == anthroModeSSE {
		a.drain()
	}
	return len(p), nil
}

// detect 按首块内容判定写出模式。
func (a *anthropicWriter) detect(p []byte) {
	s := strings.TrimLeft(string(p), " \t\r\n")
	switch {
	case strings.HasPrefix(s, "data:"), strings.HasPrefix(s, ":"):
		a.mode = anthroModeSSE
		a.stream = newAnthroStream(a.w, a.fl, a.meta)
	default:
		a.mode = anthroModeJSON
	}
}

// drain 把缓冲里已完整的 SSE 帧（以空行分隔）逐帧翻译。
func (a *anthropicWriter) drain() {
	for {
		raw := a.buf.Bytes()
		idx := bytes.Index(raw, []byte("\n\n"))
		if idx < 0 {
			return
		}
		frame := string(raw[:idx])
		a.buf.Next(idx + 2)
		a.handleFrame(frame)
	}
}

// handleFrame 从一帧里取出所有 data: 行并交给翻译器（Anthropic 与 OpenAI 都是
// 一行 data 一帧，但多行形态也不该丢）。
func (a *anthropicWriter) handleFrame(frame string) {
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue // 心跳/注释行
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		a.stream.frame(payload)
	}
}

// finish 由 handler 在 chatCompletions 返回后调用，完成收尾翻译。
func (a *anthropicWriter) finish() {
	switch a.mode {
	case anthroModeSSE:
		a.drain() // 处理末尾可能残留的半帧
		a.stream.finalize()
	case anthroModeJSON:
		a.finishJSON()
	default:
		// 上游什么都没写：给客户端一个明确的错误，而不是空响应。
		writeAnthropicError(a.w, http.StatusInternalServerError, "empty upstream response")
	}
}

// finishJSON 把缓冲的 OpenAI JSON 响应翻译成 Anthropic 形态（错误信封或 message）。
func (a *anthropicWriter) finishJSON() {
	raw := a.buf.Bytes()
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		writeAnthropicErrorCode(a.w, a.status, "invalid_upstream_response", strings.TrimSpace(string(raw)))
		return
	}
	if e, ok := obj["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		code, _ := e["code"].(string)
		if msg == "" {
			msg = "upstream error"
		}
		writeAnthropicErrorCode(a.w, a.status, code, msg)
		return
	}
	// 上游连接被掐（EOF 且无 finish_reason）：非流式还没有向客户端写过任何字节，
	// 最诚实的处置是明确报错，而不是编一条 stop_reason:"end_turn" 的"完整"消息——
	// 后者会把半截正文当最终答案喂给客户端（与流式路径同源的缺陷）。
	// TakeTruncated 顺带取走内部标记键，保证它不出现在对外响应里。
	if upstream.TakeTruncated(obj) {
		noteUpstreamTruncation("messages", a.meta.Model, false)
		writeAnthropicErrorCode(a.w, http.StatusBadGateway, "upstream_interrupted",
			"upstream stream ended before completion (no finish_reason)")
		return
	}
	writeJSON(a.w, a.status, openAIToAnthropicMessage(obj, a.meta))
}
