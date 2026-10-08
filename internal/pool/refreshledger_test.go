// refreshledger_test.go 续期健康台账（P0-1）测试。
//
// 覆盖：成功/失败记账、连击清零语义、错误摘要截断、未知 uid 空操作、以及两条
// **设计约束**——(1) 台账是运行态观测，更新它不得标脏 state.json；
// (2) needs_relogin 只对 12153 死因成立（其余 disabled 号 revive 有意义）。
package pool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// refreshLedgerSnapshot 包内只读快照 helper（同 modelCostSnapshot 风格），
// 曝露三个未导出字段供断言。
func refreshLedgerSnapshot(p *Pool, uid string) (time.Time, int, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID[uid]
	if e == nil {
		return time.Time{}, 0, ""
	}
	return e.refreshOKAt, e.refreshFailStreak, e.lastRefreshErr
}

// TestNoteRefreshOKClearsFailStreak 续期成功必须把连击与错误摘要一起清空——
// 否则"续期链路已恢复"这件事在台账上永远看不出来（连击停在历史峰值）。
func TestNoteRefreshOKClearsFailStreak(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteRefreshFail("u1", errors.New("boom"))
	p.NoteRefreshFail("u1", errors.New("boom"))
	if _, streak, _ := refreshLedgerSnapshot(p, "u1"); streak != 2 {
		t.Fatalf("连续失败计数 = %d want 2", streak)
	}

	before := time.Now()
	p.NoteRefreshOK("u1")
	okAt, streak, lastErr := refreshLedgerSnapshot(p, "u1")

	if streak != 0 {
		t.Errorf("续期成功后连击应清零, got %d", streak)
	}
	if lastErr != "" {
		t.Errorf("续期成功后错误摘要应清空, got %q", lastErr)
	}
	if okAt.Before(before) || okAt.After(time.Now()) {
		t.Errorf("refreshOKAt=%v 应落在 [%v, now] 内", okAt, before)
	}
}

// TestNoteRefreshFailTruncatesErr 错误摘要必须截断：refresh 错误里可能带整段
// 上游响应体，原样透出到 /status 会让一个响应膨胀到几十 KB。
func TestNoteRefreshFailTruncatesErr(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteRefreshFail("u1", errors.New(strings.Repeat("x", 500)))
	_, streak, lastErr := refreshLedgerSnapshot(p, "u1")

	if streak != 1 {
		t.Fatalf("连续失败计数 = %d want 1", streak)
	}
	// Truncate 按显示宽度截到 n，可能带省略号；给 10 的余量，只要求"远小于原文"。
	if w := len([]rune(lastErr)); w > 90 {
		t.Errorf("错误摘要未截断：%d runes（原文 500）", w)
	}
}

// TestNoteRefreshFailNilErrKeepsSummary 连击要涨，但 nil error 不该把已有摘要抹空
// （调用点允许传 nil 表示"失败了但没拿到 err"，此时保留上一次的可读原因更有用）。
func TestNoteRefreshFailNilErrKeepsSummary(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteRefreshFail("u1", errors.New("first"))
	p.NoteRefreshFail("u1", nil)
	_, streak, lastErr := refreshLedgerSnapshot(p, "u1")

	if streak != 2 {
		t.Fatalf("连续失败计数 = %d want 2", streak)
	}
	if lastErr != "first" {
		t.Errorf("nil err 不该清掉既有摘要, got %q", lastErr)
	}
}

// TestRefreshLedgerUnknownUIDIsNoop 未知 uid 不得凭空创建条目
// （台账是观测，不能成为"造号"的入口）。
func TestRefreshLedgerUnknownUIDIsNoop(t *testing.T) {
	p := New("")
	p.NoteRefreshOK("nope")
	p.NoteRefreshFail("nope", errors.New("x"))

	if _, _, _, found := p.RefreshLedger("nope"); found {
		t.Fatal("未知 uid 不该被创建")
	}
	if len(p.List()) != 0 {
		t.Fatal("池内不应出现新账号")
	}
}

// TestRefreshLedgerIsRuntimeOnly 台账是**运行态**观测：更新它不得标脏 state.json。
//
// 两个理由，缺一都说明设计被改坏了：
//   - 性能/寿命：token 续期是高频路径，每次落盘 fsync 纯浪费；
//   - 语义：落盘会让重启后出现"文件写着 3 天前续期成功、其实本进程还没试过"
//     这种自相矛盾的台账——比"未知"更误导。
func TestRefreshLedgerIsRuntimeOnly(t *testing.T) {
	p := New("") // stateFp 空 → 不起 flusher，dirty 只可能被显式操作置起
	p.Add(&auth.Auth{UID: "u1"})
	p.dirty.Store(false) // 抹掉 Add 造成的脏标记，隔离本用例的观测对象

	p.NoteRefreshOK("u1")
	if p.dirty.Load() {
		t.Error("NoteRefreshOK 不该标脏 state.json")
	}
	p.NoteRefreshFail("u1", errors.New("x"))
	if p.dirty.Load() {
		t.Error("NoteRefreshFail 不该标脏 state.json")
	}
}

// TestStatusExposesCredentialHealth /status 必须透出凭证判据，且 expires_at 取自
// **进程内 Auth 对象**（不是去 stat auths/*.json）——这正是控制台"显示已过期却还能用"
// 的根源：文件是快照，进程内才是续期/选号真正用的值。
func TestStatusExposesCredentialHealth(t *testing.T) {
	fp := t.TempDir() + "/u1.json"
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("准备凭证文件: %v", err)
	}
	p := New("")
	p.Add(a)
	p.NoteRefreshOK("u1")

	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	a.Lock()
	a.ExpiresAt = exp
	a.Unlock()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应存在于池内")
	}
	if st.TokenExpiresAt != exp {
		t.Errorf("token_expires_at = %d want %d", st.TokenExpiresAt, exp)
	}
	if st.TokenExpired {
		t.Error("30 天后的到期不该判为已过期")
	}
	if st.CredWrittenAt == nil {
		t.Error("cred_written_at 应非零（凭证文件刚被写入）")
	}
	if st.RefreshOKAt == nil {
		t.Error("refresh_ok_at 应非零（刚记过一次成功）")
	}
	if st.RefreshFailStreak != 0 {
		t.Errorf("refresh_fail_streak = %d want 0", st.RefreshFailStreak)
	}
	if st.NeedsRelogin {
		t.Error("健康账号不该 needs_relogin")
	}
}

// TestStatusTokenExpiredFlag 「已过期」是独立判据，且**不等于**不可用/需重登。
func TestStatusTokenExpiredFlag(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"}
	a.Lock()
	a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	a.Unlock()

	p := New("")
	p.Add(a)
	st, _ := p.Status("u1")

	if !st.TokenExpired {
		t.Error("过去的 expiresAt 应置 token_expired=true")
	}
	if st.NeedsRelogin {
		t.Error("仅 access token 过期 ≠ 需要重登（会自动续期）")
	}
	if st.Disabled {
		t.Error("仅过期不该禁用账号")
	}
}

// TestStatusCredentialJSONOmitsUnknown 未观测到的凭证时间在 JSON 里必须**缺席**，
// 而不是写成 0001-01-01T00:00:00Z——监控靠"键是否存在"区分"未知"与"很久以前"，
// 而 year-1 这个值看起来像个合法时间戳，会被静默算成"上次续期在公元 1 年"。
// 这正是项目在 stateAccount.BreakerUntil 上用 *time.Time 的同一个理由。
func TestStatusCredentialJSONOmitsUnknown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"}) // 无 FilePath、无续期台账

	st, _ := p.Status("u1")
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)

	for _, k := range []string{"cred_written_at", "refresh_ok_at"} {
		if strings.Contains(s, `"`+k+`"`) {
			t.Errorf("未观测的 %s 不该出现在 JSON 里: %s", k, s)
		}
	}
	// 恒写出的判据必须都在（运维口径：0/false 是"正常"的证据，不是"没记录"）
	for _, k := range []string{"refresh_fail_streak", "session_dead_fails", "token_expired", "needs_relogin"} {
		if !strings.Contains(s, `"`+k+`"`) {
			t.Errorf("恒写出的 %s 缺失: %s", k, s)
		}
	}

	// 一旦观测到成功续期，键就必须出现（否则"省略"会退化成"永远不输出"）
	p.NoteRefreshOK("u1")
	st2, _ := p.Status("u1")
	raw2, err := json.Marshal(st2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw2), `"refresh_ok_at"`) {
		t.Errorf("已观测到续期时 refresh_ok_at 应出现: %s", raw2)
	}
}

// TestStatusCredWrittenAtZeroWhenNoFile 文件不可 stat 时 → nil → 键不输出
// （omitempty），语义是"未知"，而不是"1970 年写的"。
func TestStatusCredWrittenAtZeroWhenNoFile(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"}) // FilePath 空
	st, _ := p.Status("u1")
	if st.CredWrittenAt != nil {
		t.Errorf("无 FilePath 时 cred_written_at 应为 nil, got %v", st.CredWrittenAt)
	}
}

// TestNeedsReloginOnlyForSessionDead 12153 达阈值被禁用 → needs_relogin=true。
// 这类号的 refresh token 已失效，revive 只会让它立刻再死，必须重登。
func TestNeedsReloginOnlyForSessionDead(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})

	for i := 0; i < SessionDeadThreshold(); i++ {
		p.NoteSessionDead("u1")
	}
	st, _ := p.Status("u1")

	if !st.Disabled {
		t.Fatal("连续 12153 达阈值应被禁用")
	}
	if !st.NeedsRelogin {
		t.Error("12153 禁用必须标 needs_relogin")
	}
}

// TestNeedsReloginFalseForOtherDisableReasons 非 12153 死因（如 WAF 封禁）的
// disabled 号，revive 是有意义的，不该被标成"只能重登"——否则运维会误弃可救的号。
func TestNeedsReloginFalseForOtherDisableReasons(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	p.Disable("u1", "waf 403 block")

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatal("应被禁用")
	}
	if st.NeedsRelogin {
		t.Error("非 12153 死因不该标 needs_relogin")
	}
	if st.DisabledReason != "waf 403 block" {
		t.Errorf("disabled_reason = %q", st.DisabledReason)
	}
}
