package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestNormalizeAPIKeys 分组密钥表的 fail-fast 三则（空密钥 / 非法组名 / 密钥重复）。
func TestNormalizeAPIKeys(t *testing.T) {
	cases := []struct {
		name    string
		apiKey  string
		in      []APIKeyEntry
		wantErr string // 非空 = 期望报错且错误信息包含该子串
	}{
		{"不配 api_keys 合法（纯增量）", "sk-main", nil, ""},
		{"空数组合法（等于不分组）", "sk-main", []APIKeyEntry{}, ""},
		{"正常两把分组密钥", "sk-main", []APIKeyEntry{
			{Key: "sk-i", Name: "内部", Groups: []string{"internal"}},
			{Key: "sk-e", Name: "外部", Groups: []string{"external"}},
		}, ""},
		{"groups 省略 = 不限分组（合法）", "sk-main", []APIKeyEntry{
			{Key: "sk-i", Name: "内部"},
		}, ""},
		{"空密钥报错", "sk-main", []APIKeyEntry{
			{Key: "   ", Name: "坏条目"},
		}, "key 为空"},
		{"组名含大写报错", "sk-main", []APIKeyEntry{
			{Key: "sk-i", Groups: []string{"Internal"}},
		}, "非法组名"},
		{"组名含空格报错", "sk-main", []APIKeyEntry{
			{Key: "sk-i", Groups: []string{"in ternal"}},
		}, "非法组名"},
		{"组名超长报错", "sk-main", []APIKeyEntry{
			{Key: "sk-i", Groups: []string{strings.Repeat("a", 33)}},
		}, "非法组名"},
		{"组名以连字符开头报错", "sk-main", []APIKeyEntry{
			{Key: "sk-i", Groups: []string{"-bad"}},
		}, "非法组名"},
		{"与主密钥重复报错", "sk-main", []APIKeyEntry{
			{Key: "sk-main", Name: "重复"},
		}, "密钥相同"},
		{"两条之间重复报错", "sk-main", []APIKeyEntry{
			{Key: "sk-x", Name: "甲"},
			{Key: "sk-x", Name: "乙"},
		}, "密钥相同"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{APIKey: c.apiKey, APIKeys: c.in}
			err := cfg.normalizeAPIKeys()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("不该报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错（含 %q），实得 nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestNormalizeAPIKeysGroupNormalization 组名归一：去首尾空白、丢空串、按首次出现去重。
func TestNormalizeAPIKeysGroupNormalization(t *testing.T) {
	cfg := &Config{APIKey: "sk-main", APIKeys: []APIKeyEntry{
		{Key: "sk-i", Groups: []string{" internal ", "", "internal", "external", "  "}},
	}}
	if err := cfg.normalizeAPIKeys(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []string{"internal", "external"}
	if !reflect.DeepEqual(cfg.APIKeys[0].Groups, want) {
		t.Errorf("Groups=%#v want %#v", cfg.APIKeys[0].Groups, want)
	}
}

// TestNormalizeAPIKeysEmptyGroupsStaysNil 全空白组名收敛成 nil（= 不限分组），
// 而不是留下一个长度为 0 的切片（两者在 JSON 序列化后形态不同，会污染配置文件 diff）。
func TestNormalizeAPIKeysEmptyGroupsStaysNil(t *testing.T) {
	cfg := &Config{APIKey: "sk-main", APIKeys: []APIKeyEntry{
		{Key: "sk-i", Groups: []string{"  ", ""}},
	}}
	if err := cfg.normalizeAPIKeys(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.APIKeys[0].Groups != nil {
		t.Errorf("全空白组名应收敛为 nil，实得 %#v", cfg.APIKeys[0].Groups)
	}
}

// TestNormalizeAPIKeysTrimsName 名称去首尾空白（仅展示用，不参与匹配）。
func TestNormalizeAPIKeysTrimsName(t *testing.T) {
	cfg := &Config{APIKey: "sk-main", APIKeys: []APIKeyEntry{
		{Key: "  sk-i  ", Name: "  内部  ", Groups: []string{"internal"}},
	}}
	if err := cfg.normalizeAPIKeys(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.APIKeys[0].Key != "sk-i" || cfg.APIKeys[0].Name != "内部" {
		t.Errorf("Key/Name 未 trim: %+v", cfg.APIKeys[0])
	}
}

// TestBuildAuthKeys 展开顺序与语义：主密钥排第一且不限分组（老客户端零回归）。
func TestBuildAuthKeys(t *testing.T) {
	cfg := &Config{
		APIKey: "sk-main",
		APIKeys: []APIKeyEntry{
			{Key: "sk-i", Name: "内部", Groups: []string{"internal"}},
			{Key: "sk-e", Name: "外部", Groups: []string{"external"}},
		},
	}
	got := buildAuthKeys(cfg)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3（主密钥 + 2 分组密钥）", len(got))
	}
	if got[0].Key != "sk-main" {
		t.Errorf("主密钥必须排第一，实得 %q", got[0].Key)
	}
	if len(got[0].Groups) != 0 {
		t.Errorf("主密钥必须不限分组，实得 %v", got[0].Groups)
	}
	if got[1].Key != "sk-i" || !reflect.DeepEqual(got[1].Groups, []string{"internal"}) {
		t.Errorf("分组密钥条目异常: %+v", got[1])
	}
	if got[2].Key != "sk-e" || !reflect.DeepEqual(got[2].Groups, []string{"external"}) {
		t.Errorf("分组密钥条目异常: %+v", got[2])
	}
}

// TestBuildAuthKeysEmpty 全空 → nil（不鉴权部署语义，不得变成空切片）。
func TestBuildAuthKeysEmpty(t *testing.T) {
	if got := buildAuthKeys(&Config{}); got != nil {
		t.Errorf("全空应返回 nil，实得 %#v", got)
	}
	// 只配分组密钥、无主密钥：合法（纯分组部署），此时不限分组的客户端没有密钥可用。
	got := buildAuthKeys(&Config{APIKeys: []APIKeyEntry{{Key: "sk-i", Groups: []string{"internal"}}}})
	if len(got) != 1 || got[0].Key != "sk-i" {
		t.Errorf("纯分组部署应展开 1 条，实得 %#v", got)
	}
}

// TestAPIKeyFPStable 指纹稳定且不回显明文（错误信息里也不得出现密钥本体）。
func TestAPIKeyFPStable(t *testing.T) {
	a := apiKeyFP("sk-wb2a-abcdef")
	b := apiKeyFP("sk-wb2a-abcdef")
	if a != b || len(a) != 16 {
		t.Fatalf("指纹不稳定或长度异常: %q vs %q", a, b)
	}
	if apiKeyFP("sk-wb2a-abcdeF") == a {
		t.Error("不同密钥不该同指纹")
	}
	// 重复密钥的报错信息里只应有指纹，不应有密钥明文
	cfg := &Config{APIKey: "sk-secret-value", APIKeys: []APIKeyEntry{{Key: "sk-secret-value"}}}
	err := cfg.normalizeAPIKeys()
	if err == nil {
		t.Fatal("应报重复")
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Errorf("错误信息泄露了密钥明文: %v", err)
	}
}
