package auth

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseGroupsTopLevel 顶层 groups 在两种磁盘形态下都要能读出来。
func TestParseGroupsTopLevel(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			"嵌套形",
			`{"auth":{"accessToken":"at"},"account":{"uid":"u1"},"groups":["internal","external"]}`,
			[]string{"internal", "external"},
		},
		{
			"扁平形",
			`{"accessToken":"at","uid":"u2","groups":["external"]}`,
			[]string{"external"},
		},
		{
			"无 groups 键（存量老文件）",
			`{"accessToken":"at","uid":"u3"}`,
			nil,
		},
		{
			"归一：去首尾空白 + 丢空串 + 按首次出现去重",
			`{"accessToken":"at","uid":"u4","groups":[" internal ","","internal","external","  "]}`,
			[]string{"internal", "external"},
		},
		{
			"全为空白 → nil",
			`{"accessToken":"at","uid":"u5","groups":["  ",""]}`,
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := Parse([]byte(c.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !reflect.DeepEqual(a.Groups, c.want) {
				t.Errorf("Groups=%#v want %#v", a.Groups, c.want)
			}
		})
	}
}

// TestSaveAtomicKeepsGroups 是**分组功能最关键的防回归用例**。
//
// SaveAtomic 用固定字段集**整体重写**文件，漏写 groups 就等于每次 token 刷新都静默
// 抹掉分组标签 —— 而 refresh 是高频路径，表现为"分组某天开始拿不到号"却查无实据。
func TestSaveAtomicKeepsGroups(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", UID: "u1", FilePath: fp,
		Groups: []string{"internal", "external"}}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if !reflect.DeepEqual(b.Groups, []string{"internal", "external"}) {
		t.Errorf("SaveAtomic 后 groups 丢失/变化: %#v\nraw=%s", b.Groups, raw)
	}
}

// TestSaveAtomicOmitsEmptyGroups 无分组时不写 groups 键（不给存量文件引入空键）。
func TestSaveAtomicOmitsEmptyGroups(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u2.json")
	a := &Auth{AccessToken: "at", UID: "u2", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, _ := os.ReadFile(fp)
	if strings.Contains(string(raw), "groups") {
		t.Errorf("空 Groups 不该写回 groups 键: %s", raw)
	}
}

// TestMatchesGroups 分组可见性矩阵 —— 本次改动的核心语义。
func TestMatchesGroups(t *testing.T) {
	cases := []struct {
		name     string
		acc      []string
		want     []string
		expected bool
	}{
		{"密钥不限分组 → 打标签账号可见", []string{"internal"}, nil, true},
		{"密钥不限分组 → 未打标签账号也可见", nil, nil, true},
		{"密钥不限分组（空切片）", []string{"a"}, []string{}, true},
		{"交集命中", []string{"internal", "external"}, []string{"external"}, true},
		{"无交集 → 不可见", []string{"internal"}, []string{"external"}, false},
		{"账号未分组 → 默认拒绝", nil, []string{"internal"}, false},
		{"账号未分组 → 多组也不可见", nil, []string{"internal", "external"}, false},
		{"多对多交集", []string{"a", "b", "c"}, []string{"c", "d"}, true},
		{"大小写敏感（精确相等）", []string{"Internal"}, []string{"internal"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{Groups: c.acc}
			if got := a.MatchesGroups(c.want); got != c.expected {
				t.Errorf("MatchesGroups(%v) with acc=%v = %v want %v", c.want, c.acc, got, c.expected)
			}
		})
	}

	// nil 接收者不得 panic：不限分组应放行，指定分组应拒绝。
	var nilAuth *Auth
	if !nilAuth.MatchesGroups(nil) {
		t.Error("nil *Auth 在不限分组时应放行")
	}
	if nilAuth.MatchesGroups([]string{"x"}) {
		t.Error("nil *Auth 在指定分组时应拒绝")
	}
}

// TestGroupsValueIsCopy 返回副本，调用方改动不得影响内部切片。
func TestGroupsValueIsCopy(t *testing.T) {
	a := &Auth{Groups: []string{"internal"}}
	got := a.GroupsValue()
	if len(got) != 1 {
		t.Fatalf("GroupsValue=%v", got)
	}
	got[0] = "mutated"
	if a.Groups[0] != "internal" {
		t.Errorf("GroupsValue 返回的是内部切片本体（已被外部改成 %q）", a.Groups[0])
	}
	if (*Auth)(nil).GroupsValue() != nil {
		t.Error("nil 接收者应返回 nil")
	}
}

// TestLoadDirKeepsGroups 目录加载路径同样保留分组（Parse → LoadDir 全链路）。
func TestLoadDirKeepsGroups(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u9.json")
	raw := `{"auth":{"accessToken":"at","realm":"cn"},"account":{"uid":"u9"},"groups":["internal"]}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len=%d want 1", len(list))
	}
	if !reflect.DeepEqual(list[0].Groups, []string{"internal"}) {
		t.Errorf("LoadDir 后 Groups=%#v want [internal]", list[0].Groups)
	}
}
