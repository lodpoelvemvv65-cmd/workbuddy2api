// clientsource.go 识别请求来自哪个客户端（面板「最近请求日志」的来源列）。
//
// 为什么要猜而不是要求上报：网关要服务的正是这些第三方 CLI / SDK / 桌面端，它们
// 不会为本网关改代码。能稳定拿到的信号只有各家固定的请求头（User-Agent、
// originator、x-app…），这里把它们归一成短标签，供面板一眼看出「谁在调」。
//
// 设计纪律：
//   - **纯函数、无副作用**：detectClientSource 只读 header + path，方便单测；
//   - **可自报**：未收录的客户端带 `X-Client-Source: my-desktop-cli` 即可零改网关
//     出现在面板上，优先级最高；
//   - **永不 panic / 不落库**：仅用于展示，取不到就回落协议名，最差返回 ""（面板显示 "-"）。
package server

import (
	"net/http"
	"strings"
)

// clientSourceHeaderNames 显式自报头（按优先级）。客户端主动声明自己是谁，
// 覆盖一切启发式判断，给自研 / 小众客户端留的零改网关入口。
var clientSourceHeaderNames = []string{"X-Client-Source", "X-Client-Name"}

// detectClientSource 推断本次请求的客户端来源标签（小写、短、可直接展示）。
//
// 优先级：显式自报 > originator（Codex 系自报）> User-Agent 专有特征 > x-app >
// User-Agent 首个产品 token > 协议路径回落。完全无从判断时返回 ""。
func detectClientSource(h http.Header, path string) string {
	if s := explicitClientSource(h); s != "" {
		return s
	}
	ua := strings.ToLower(strings.TrimSpace(h.Get("User-Agent")))
	if s := matchOriginator(strings.ToLower(strings.TrimSpace(h.Get("Originator")))); s != "" {
		return s
	}
	if s := matchUserAgent(ua); s != "" {
		return s
	}
	if s := matchXApp(strings.ToLower(strings.TrimSpace(h.Get("X-App")))); s != "" {
		return s
	}
	if s := fallbackFromUA(ua); s != "" {
		return s
	}
	return sourceFromPath(path)
}

// explicitClientSource 读显式自报头；非法/空值跳过。
func explicitClientSource(h http.Header) string {
	for _, name := range clientSourceHeaderNames {
		if v := sanitizeSourceLabel(h.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// sanitizeSourceLabel 归一标签：去首尾空白、转小写、只留安全可见字符、限长 32。
// 目的是防止把控制字符 / 超长串写进面板 HTML（面板另有转义，这里再收一道）。
// 全为分隔符（如 "---"）视作未提供，返回 ""。
func sanitizeSourceLabel(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ':
			b.WriteRune(r)
		}
		if b.Len() >= 32 {
			break
		}
	}
	out := strings.ToLower(strings.TrimSpace(b.String()))
	if strings.Trim(out, "-_. ") == "" {
		return ""
	}
	return out
}

// matchOriginator 识别 originator 头（Codex 系用它标明来源）。未知值不采信，
// 留给 User-Agent 继续判断（originator 是半私有头，避免被任意值带偏）。
func matchOriginator(orig string) string {
	switch {
	case orig == "":
		return ""
	case strings.Contains(orig, "codex_vscode"):
		return "codex-vscode"
	case strings.Contains(orig, "codex"):
		return "codex-cli"
	case strings.Contains(orig, "claude"):
		return "claude-code"
	}
	return ""
}

// matchUserAgent 按 User-Agent 特征匹配常见客户端。顺序即优先级：越专有的越靠前，
// 避免 "node" 之类通用 token 把 "openai-node" 抢走。
func matchUserAgent(ua string) string {
	if ua == "" {
		return ""
	}
	switch {
	// ── 编程 CLI / 桌面 Agent（本项目的主要调用方）────────────────────────
	case strings.Contains(ua, "claude-cli"), strings.Contains(ua, "claude-code"):
		return "claude-code"
	case strings.Contains(ua, "codex_cli_rs"), strings.Contains(ua, "codex-cli"), strings.Contains(ua, "codex_cli"):
		return "codex-cli"
	case strings.Contains(ua, "codex_vscode"):
		return "codex-vscode"
	case strings.HasPrefix(ua, "pi ("), strings.Contains(ua, "pi-coding-agent"), strings.Contains(ua, "@earendil-works/pi"):
		return "pi-cli"
	case strings.Contains(ua, "opencode"):
		return "opencode"
	case strings.Contains(ua, "cursor"):
		return "cursor"
	case strings.Contains(ua, "windsurf"):
		return "windsurf"
	case strings.Contains(ua, "cline"):
		return "cline"
	case strings.Contains(ua, "roo-code"), strings.Contains(ua, "roocode"):
		return "roo-code"
	case strings.Contains(ua, "aider"):
		return "aider"
	// ── 聊天 / 桌面客户端 ────────────────────────────────────────────────
	case strings.Contains(ua, "cherry studio"), strings.Contains(ua, "cherrystudio"):
		return "cherry-studio"
	case strings.Contains(ua, "chatbox"):
		return "chatbox"
	case strings.Contains(ua, "lobe-chat"), strings.Contains(ua, "lobehub"):
		return "lobe-chat"
	case strings.Contains(ua, "open-webui"):
		return "open-webui"
	case strings.Contains(ua, "nextchat"):
		return "nextchat"
	// ── 官方 / 通用 SDK 与运行时 ─────────────────────────────────────────
	case strings.Contains(ua, "openai-python"), strings.Contains(ua, "openai/python"):
		return "openai-python"
	case strings.Contains(ua, "openai-node"), strings.Contains(ua, "openai/node"):
		return "openai-node"
	case strings.Contains(ua, "anthropic/python"), strings.Contains(ua, "anthropic-python"):
		return "anthropic-python"
	case strings.Contains(ua, "anthropic/typescript"), strings.Contains(ua, "anthropic-sdk-typescript"):
		return "anthropic-node"
	case strings.Contains(ua, "go-http-client"):
		return "go"
	case strings.Contains(ua, "python-requests"), strings.Contains(ua, "python-urllib"), strings.Contains(ua, "httpx"):
		return "python"
	case strings.HasPrefix(ua, "curl/"):
		return "curl"
	case strings.Contains(ua, "okhttp"):
		return "okhttp"
	case strings.Contains(ua, "axios"):
		return "axios"
	case strings.Contains(ua, "undici"), strings.Contains(ua, "node-fetch"):
		return "node"
	}
	return ""
}

// matchXApp 识别 x-app 头（部分客户端用它声明应用形态）。只采信明确值。
func matchXApp(app string) string {
	switch app {
	case "cli":
		return "cli"
	case "vscode":
		return "vscode"
	case "jetbrains":
		return "jetbrains"
	case "desktop":
		return "desktop"
	}
	return ""
}

// fallbackFromUA 取 User-Agent 首个产品 token（`MyDesktop/1.2 (linux)` → `mydesktop`），
// 让未收录的客户端（自研 desktop cli 等）也能显示大致来源，而不是一律 "-"。
// 浏览器 / 通用运行时的 token 无辨识度，直接放弃（回落协议名更有意义）。
func fallbackFromUA(ua string) string {
	if ua == "" {
		return ""
	}
	tok := ua
	if i := strings.IndexAny(tok, " \t"); i >= 0 {
		tok = tok[:i]
	}
	if i := strings.IndexByte(tok, '/'); i >= 0 {
		tok = tok[:i]
	}
	tok = sanitizeSourceLabel(tok)
	switch tok {
	case "", "mozilla", "node", "python", "go", "curl", "axios", "okhttp":
		return ""
	}
	return tok
}

// sourceFromPath 在没有任何客户端特征头时，按协议入口回落：
// /v1/messages → anthropic，/v1/responses → responses，chat 入口 → openai。
func sourceFromPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return "anthropic"
	case strings.HasPrefix(path, "/v1/responses"):
		return "responses"
	case strings.HasPrefix(path, "/v1/chat/completions"):
		return "openai"
	}
	return ""
}
