// metrics_cred_test.go 账号凭证健康指标（P0-1，逐账号）的导出测试。
//
// 锁住四件事：
//  1. 四个家族都在，且缺失值语义正确（无 expiry 时**不输出**该样本，而不是写 0）；
//  2. 输出稳定（同输入两次渲染逐字节相等、乱序输入仍按 account 字典序）；
//  3. 标签只带 uid 前 8 位——不得泄漏完整 uid（本文件原先的纪律，破例加账号
//     维度的前提就是"不泄漏完整标识"）；
//  4. credential_last_refresh 取**凭证文件 mtime**（重启可存活），不是进程内时刻。
package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestPromAccountCredentialFamilies 四个家族齐全 + 缺失值不写 0 + 输出稳定。
func TestPromAccountCredentialFamilies(t *testing.T) {
	creds := []promAccountCred{
		{label: "aaaaaaaa", realm: "cn", expiresAt: 1800000000, lastWrite: 1700000000},
		{label: "bbbbbbbb", realm: "cn", failStreak: 5, needsRelog: true}, // expiresAt/lastWrite 未知
	}

	out := writePromMetrics(MetricsSnapshot{}, nil, 0, 0, false, nil, creds)

	for _, fam := range []string{
		"wb2api_account_token_expires_seconds",
		"wb2api_account_credential_last_refresh_seconds",
		"wb2api_account_refresh_fail_streak",
		"wb2api_account_needs_relogin",
	} {
		if !strings.Contains(out, "# TYPE "+fam+" gauge") {
			t.Errorf("缺少指标家族 %s", fam)
		}
	}

	// 未知到期不输出该样本："缺失"比 "0（=1970 年）" 诚实——后者会造出
	// "这个账号凭证 55 年没更新过"的假告警。
	if strings.Contains(out, `wb2api_account_token_expires_seconds{account="bbbbbbbb"`) {
		t.Error("expiresAt=0 不该输出到期样本")
	}
	if strings.Contains(out, `wb2api_account_credential_last_refresh_seconds{account="bbbbbbbb"`) {
		t.Error("lastWrite=0（凭证文件不存在）不该输出写回时刻样本")
	}

	// 有值的账号照常输出。注意数值走 formatPromValue 的 'g' 格式——大数会写成
	// 科学计数法（1800000000 → 1.8e+09），这是 Prometheus 文本格式接受的形态，
	// 故这里只断言"样本存在"而不写死数值字面量。
	if !strings.Contains(out, `wb2api_account_token_expires_seconds{account="aaaaaaaa",realm="cn"}`) {
		t.Error("有值账号的到期样本缺失")
	}
	if !strings.Contains(out, `wb2api_account_credential_last_refresh_seconds{account="aaaaaaaa",realm="cn"}`) {
		t.Error("有值账号的写回时刻样本缺失")
	}
	if !strings.Contains(out, `wb2api_account_refresh_fail_streak{account="bbbbbbbb",realm="cn"} 5`) {
		t.Error("失败连击样本缺失或值错")
	}
	if !strings.Contains(out, `wb2api_account_needs_relogin{account="bbbbbbbb",realm="cn"} 1`) {
		t.Error("needs_relogin 样本缺失或值错")
	}
	// 健康账号的连击必须是 0（显式输出，代表"续期一直正常"的证据）
	if !strings.Contains(out, `wb2api_account_refresh_fail_streak{account="aaaaaaaa",realm="cn"} 0`) {
		t.Error("健康账号的 fail_streak 应显式输出 0")
	}

	// 稳定输出：同输入两次渲染逐字节相等（本文件对确定性输出的既有要求）。
	if again := writePromMetrics(MetricsSnapshot{}, nil, 0, 0, false, nil, creds); again != out {
		t.Error("两次渲染输出不一致（输出不稳定）")
	}

	// 乱序输入仍按 account 字典序输出（不依赖调用方顺序）
	rev := writePromMetrics(MetricsSnapshot{}, nil, 0, 0, false, nil, []promAccountCred{creds[1], creds[0]})
	ia := strings.Index(rev, `wb2api_account_needs_relogin{account="aaaaaaaa"`)
	ib := strings.Index(rev, `wb2api_account_needs_relogin{account="bbbbbbbb"`)
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("输出未按 account 标签字典序（ia=%d ib=%d）", ia, ib)
	}
}

// TestPromMetricsAccountLabelIsUID8 端到端：标签只带 uid 前 8 位，完整 uid 不出现在
// 任何一行——这是"破例允许账号维度标签"的前提条件。
func TestPromMetricsAccountLabelIsUID8(t *testing.T) {
	full := "abcdef0123456789abcdef0123456789"
	p := testPoolWith(&auth.Auth{UID: full, AccessToken: "at", RefreshToken: "rt"})
	h := NewHandler(Config{Pool: p, APIKey: "k", MetricsEnabled: true})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `account="abcdef01"`) {
		t.Error("应输出 uid 前 8 位作为 account 标签")
	}
	if strings.Contains(body, full) {
		t.Error("不得泄漏完整 uid")
	}
	if strings.Contains(body, "abcdef0123456789") {
		t.Error("不得泄漏 uid 的第 9 位及以后")
	}
}

// TestPromMetricsEndpointDisabledByDefault /metrics 默认关闭时路由不注册，
// 带正确 key 也只得 404（不暴露指标面存在）。
func TestPromMetricsEndpointDisabledByDefault(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k"}) // MetricsEnabled 零值 false

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 when metrics disabled", rec.Code)
	}
}

// TestPromCredentialLastRefreshUsesFileMtime 锁定语义：该指标取**凭证文件 mtime**
// （重启后依然可读），而**不是**进程内的 refresh_ok_at（重启清零）。
//
// 为什么必须锁：global 账号的 access token exp 在 ~1 年后，请求前的 NeedsRefresh(10m)
// 几乎不会命中，只有每日保活（keepalive_hours）才写文件。若该指标用进程内时刻，
// 每次重启后这一序列会空白数小时，监控上无法区分「网关刚重启」与「保活坏了」——
// 而这正是本指标存在的唯一理由。
func TestPromCredentialLastRefreshUsesFileMtime(t *testing.T) {
	uid := "aaaaaaaa-1111-2222-3333-444444444444"
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-"+uid+".json")
	body := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1800000000},` +
		`"account":{"uid":"` + uid + `"}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// 固定 mtime 到保活时刻（22:00），模拟"文件由昨晚保活写回"。
	want := time.Date(2026, 9, 27, 22, 0, 0, 0, time.UTC)
	if err := os.Chtimes(fp, want, want); err != nil {
		t.Fatal(err)
	}

	p := testPoolWith(&auth.Auth{
		UID: uid, AccessToken: "at", RefreshToken: "rt", FilePath: fp,
	})
	h := NewHandler(Config{Pool: p, APIKey: "k", MetricsEnabled: true})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics code=%d body=%s", rec.Code, rec.Body)
	}

	// 该序列**必须在**（凭证文件存在 → 有值），且值等于文件 mtime。
	got := promSampleValue(t, rec.Body.String(), "wb2api_account_credential_last_refresh_seconds", "aaaaaaaa")
	if got != float64(want.Unix()) {
		t.Errorf("credential_last_refresh = %v, want %d（应取文件 mtime 而非进程内时刻）", got, want.Unix())
	}

	// 反向：本进程从未发生过续期（台账 okAt 为零值）→ 该序列的存在只能是 mtime 来源。
	// 这正是本测试的意义：证明读数不是来自 RefreshOKAt。
	if okAt, _, _, found := p.RefreshLedger(uid); !found || !okAt.IsZero() {
		t.Fatalf("前置条件：本进程不应发生过续期（found=%v okAt=%v）", found, okAt)
	}
}

// promSampleValue 从 text exposition 里取指定家族 + 指定 account 标签的样本数值。
// promWriter 用 'g' 格式输出浮点，大数会写成科学计数法，故必须解析而非字符串比对。
func promSampleValue(t *testing.T, out, family, account string) float64 {
	t.Helper()
	prefix := fmt.Sprintf("%s{account=%q", family, account)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("样本行格式异常: %q", line)
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("样本值解析失败 %q: %v", line, err)
		}
		return v
	}
	t.Fatalf("未找到样本 %s{account=%q}", family, account)
	return 0
}
