package server

import (
	"net/http"
	"testing"
)

func TestDetectClientSource(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		path   string
		want   string
	}{
		// 显式自报最高优先（未收录客户端零改网关入口）。
		{
			name:   "explicit X-Client-Source 覆盖一切",
			header: map[string]string{"X-Client-Source": "My-Desktop-CLI", "User-Agent": "claude-cli/2.0.5"},
			path:   "/v1/messages",
			want:   "my-desktop-cli",
		},
		{
			name:   "显式 X-Client-Name 同样生效",
			header: map[string]string{"X-Client-Name": "acme_desktop"},
			path:   "/v1/chat/completions",
			want:   "acme_desktop",
		},
		{
			name:   "显式自报非法值跳过并回落",
			header: map[string]string{"X-Client-Source": "   ", "User-Agent": "claude-cli/2.0.5"},
			path:   "/v1/messages",
			want:   "claude-code",
		},
		// 编程 CLI。
		{
			name:   "Claude Code CLI",
			header: map[string]string{"User-Agent": "claude-cli/2.0.5 (external, cli)", "X-App": "cli"},
			path:   "/v1/messages",
			want:   "claude-code",
		},
		{
			name:   "Codex CLI 由 UA 识别",
			header: map[string]string{"User-Agent": "codex_cli_rs/0.42.0 (Mac OS 15.0; arm64) Apple_Terminal"},
			path:   "/v1/responses",
			want:   "codex-cli",
		},
		{
			name:   "Codex CLI 仅 originator 也能识别",
			header: map[string]string{"Originator": "codex_cli_rs", "User-Agent": "curl/8.7.1"},
			path:   "/v1/responses",
			want:   "codex-cli",
		},
		{
			name:   "Codex VSCode 扩展",
			header: map[string]string{"Originator": "codex_vscode"},
			path:   "/v1/responses",
			want:   "codex-vscode",
		},
		{
			name:   "pi CLI（真实 UA 前缀）",
			header: map[string]string{"User-Agent": "pi (linux 6.8.0; x64) node/24.14.0"},
			path:   "/v1/chat/completions",
			want:   "pi-cli",
		},
		{
			name:   "pi CLI（cloudflare 形态 UA）",
			header: map[string]string{"User-Agent": "pi-coding-agent"},
			path:   "/v1/chat/completions",
			want:   "pi-cli",
		},
		// 常见桌面 / 编辑器客户端。
		{
			name:   "Cursor",
			header: map[string]string{"User-Agent": "Cursor/0.42.3"},
			path:   "/v1/chat/completions",
			want:   "cursor",
		},
		{
			name:   "Cherry Studio",
			header: map[string]string{"User-Agent": "Cherry Studio/1.2.3"},
			path:   "/v1/chat/completions",
			want:   "cherry-studio",
		},
		// SDK / 运行时。
		{
			name:   "OpenAI Python SDK",
			header: map[string]string{"User-Agent": "OpenAI/Python 1.40.0"},
			path:   "/v1/chat/completions",
			want:   "openai-python",
		},
		{
			name:   "Anthropic TypeScript SDK",
			header: map[string]string{"User-Agent": "anthropic-sdk-typescript/0.30.0"},
			path:   "/v1/messages",
			want:   "anthropic-node",
		},
		{
			name:   "curl",
			header: map[string]string{"User-Agent": "curl/8.7.1"},
			path:   "/v1/chat/completions",
			want:   "curl",
		},
		// 未收录客户端 → 取 UA 产品 token。
		{
			name:   "未收录桌面端取产品名",
			header: map[string]string{"User-Agent": "SomeDesktopCLI/3.1.0 (Windows NT 10.0)"},
			path:   "/v1/chat/completions",
			want:   "somedesktopcli",
		},
		// 无特征头 → 协议回落。
		{
			name:   "无 UA 回落 anthropic 协议",
			header: map[string]string{},
			path:   "/v1/messages",
			want:   "anthropic",
		},
		{
			name:   "无 UA 回落 responses 协议",
			header: map[string]string{},
			path:   "/v1/responses",
			want:   "responses",
		},
		{
			name:   "无 UA 回落 openai 协议",
			header: map[string]string{},
			path:   "/v1/chat/completions",
			want:   "openai",
		},
		{
			name:   "浏览器 UA 不展示产品名，回落协议",
			header: map[string]string{"User-Agent": "Mozilla/5.0 (X11; Linux x86_64) Chrome/120"},
			path:   "/v1/chat/completions",
			want:   "openai",
		},
		{
			name:   "完全无从判断返回空",
			header: map[string]string{},
			path:   "/unknown",
			want:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range c.header {
				h.Set(k, v)
			}
			if got := detectClientSource(h, c.path); got != c.want {
				t.Errorf("detectClientSource(%v, %q)=%q want %q", c.header, c.path, got, c.want)
			}
		})
	}
}

// TestSanitizeSourceLabel 自报标签必须被收敛：小写、限长、剔除控制字符/HTML 危险字符。
func TestSanitizeSourceLabel(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"   ":              "",
		"My Desktop CLI":   "my desktop cli",
		"a<b>c":            "abc",
		"x\ny\tz":          "xyz",
		"---":              "",
		"../../etc/passwd": "....etcpasswd",
		"abcdefghij0123456789abcdefghij0123456789": "abcdefghij0123456789abcdefghij01", // 截到 32
	}
	for in, want := range cases {
		if got := sanitizeSourceLabel(in); got != want {
			t.Errorf("sanitizeSourceLabel(%q)=%q want %q", in, got, want)
		}
	}
}
