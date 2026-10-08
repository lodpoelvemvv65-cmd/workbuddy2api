package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// groupsPool 构造一个带业务分组标签的池（全部 cn 域，把 realm 维度排除在外，
// 使本文件的用例只考察 groups 维度）。
//
//	acc_internal  groups=[internal]
//	acc_external  groups=[external]
//	acc_both      groups=[internal,external]   ← "一个账号被两把密钥共用"的形态
//	acc_none      groups=nil                    ← 未分组（默认拒绝）
func groupsPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "acc_internal", Groups: []string{"internal"}})
	p.Add(&auth.Auth{UID: "acc_external", Groups: []string{"external"}})
	p.Add(&auth.Auth{UID: "acc_both", Groups: []string{"internal", "external"}})
	p.Add(&auth.Auth{UID: "acc_none"})
	return p
}

// TestPickExcludingForRealmGroups 选号按分组过滤：反复采样，命中集合必须落在允许组内。
func TestPickExcludingForRealmGroups(t *testing.T) {
	cases := []struct {
		name   string
		groups []string
		allow  map[string]bool
	}{
		{"内部密钥", []string{"internal"},
			map[string]bool{"acc_internal": true, "acc_both": true}},
		{"外部密钥", []string{"external"},
			map[string]bool{"acc_external": true, "acc_both": true}},
		{"不限分组（主密钥）", nil,
			map[string]bool{"acc_internal": true, "acc_external": true, "acc_both": true, "acc_none": true}},
		{"不存在的组", []string{"nope"}, nil}, // 恒无候选
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := groupsPool(t)
			for i := 0; i < 200; i++ {
				acct := p.PickExcludingForRealmGroups(nil, "", "", c.groups)
				if c.allow == nil {
					if acct != nil {
						t.Fatalf("期望无候选，却选中 %s", acct.UID)
					}
					continue
				}
				if acct == nil {
					t.Fatalf("第 %d 次选号返回 nil", i)
				}
				if !c.allow[acct.UID] {
					t.Fatalf("选中了不该可见的账号 %s（groups=%v）", acct.UID, c.groups)
				}
			}
		})
	}
}

// TestPickRespectsGroupsAndRealm groups 与 realm 是 AND 关系：模型名前缀划出的域
// 与密钥划出的组必须同时满足。
func TestPickRespectsGroupsAndRealm(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "cn_internal", Domain: "", Groups: []string{"internal"}})
	p.Add(&auth.Auth{UID: "g_internal", Domain: "www.workbuddy.ai", Groups: []string{"internal"}})
	p.Add(&auth.Auth{UID: "g_external", Domain: "www.workbuddy.ai", Groups: []string{"external"}})

	// global 域 + internal 组 → 只能命中 g_internal
	for i := 0; i < 100; i++ {
		acct := p.PickExcludingForRealmGroups(nil, "", "global", []string{"internal"})
		if acct == nil {
			t.Fatalf("第 %d 次无候选（应命中 g_internal）", i)
		}
		if acct.UID != "g_internal" {
			t.Fatalf("realm=global + groups=[internal] 命中了 %s（应为 g_internal）", acct.UID)
		}
	}

	// cn 域 + internal 组 → 只能命中 cn_internal
	for i := 0; i < 100; i++ {
		acct := p.PickExcludingForRealmGroups(nil, "", "cn", []string{"internal"})
		if acct == nil || acct.UID != "cn_internal" {
			t.Fatalf("realm=cn + groups=[internal] 命中 %v（应为 cn_internal）", acct)
		}
	}

	// cn 域 + external 组 → 池内无此组合 → 无候选
	if acct := p.PickExcludingForRealmGroups(nil, "", "cn", []string{"external"}); acct != nil {
		t.Fatalf("cn+external 不该有候选，却选中 %s", acct.UID)
	}
}

// TestFallbackRespectsGroups 是**隔离漏洞的防回归用例**：
// 本组账号全部冷却、正常路径无候选时，全冷却兜底**不得**从其他分组借号。
//
// 这是最容易漏的一处：隔离若只在正常路径生效，就会在"本组资源最紧张"的时刻失效，
// 而日志上只表现为一次正常的 fallback_earliest_expiry，看不出串组。
func TestFallbackRespectsGroups(t *testing.T) {
	p := groupsPool(t)
	// 让 internal 组（acc_internal + acc_both）全部进入软冷却 → 内部密钥无 healthy 候选，
	// 必然走兜底路径；此时 external 组的账号仍健康可借。
	until := time.Now().Add(10 * time.Minute)
	for _, uid := range []string{"acc_internal", "acc_both"} {
		p.CooldownSoftRate(uid, 10*time.Minute, until, "test 制造全冷却")
	}
	for i := 0; i < 200; i++ {
		acct := p.PickExcludingForRealmGroups(nil, "", "", []string{"internal"})
		if acct == nil {
			continue // 兜底允许无候选（这也是可接受的结果）
		}
		if !authOf(acct).MatchesGroups([]string{"internal"}) {
			t.Fatalf("兜底从其他分组借号了：%s —— 分组隔离失效", acct.UID)
		}
	}
}

// TestPickByUIDForModelGroups 粘性命中路径同样受分组约束（会话复用不同密钥时不得越组）。
func TestPickByUIDForModelGroups(t *testing.T) {
	p := groupsPool(t)
	if got := p.PickByUIDForModelGroups("acc_internal", "", []string{"internal"}); got == nil {
		t.Error("本组账号的粘性命中应成功")
	}
	if got := p.PickByUIDForModelGroups("acc_internal", "", []string{"external"}); got != nil {
		t.Error("跨组账号的粘性命中必须失败（否则隔离被粘性路径绕过）")
	}
	if got := p.PickByUIDForModelGroups("acc_none", "", []string{"internal"}); got != nil {
		t.Error("未分组账号对分组密钥必须不可见")
	}
	if got := p.PickByUIDForModelGroups("acc_external", "", nil); got == nil {
		t.Error("不限分组时应命中任意账号")
	}
	// 老签名 = 不限分组，语义必须等价（零回归）
	if got := p.PickByUIDForModel("acc_external", ""); got == nil {
		t.Error("PickByUIDForModel 应等价于不限分组")
	}
}

// TestPickScopedBackCompat 空 groups 与旧入口 PickExcludingForRealm 语义等价。
func TestPickScopedBackCompat(t *testing.T) {
	p := groupsPool(t)
	for i := 0; i < 100; i++ {
		if got := p.PickExcludingForRealmGroups(nil, "", "", nil); got == nil {
			t.Fatal("空 groups 应退化为不限分组，不得无候选")
		}
		if got := p.PickExcludingForRealm(nil, "", ""); got == nil {
			t.Fatal("旧入口应保持可用（零回归）")
		}
	}
}

// authOf 把选出的 *auth.Auth 原样返回，仅用于让断言读起来更直白。
func authOf(a *auth.Auth) *auth.Auth { return a }
