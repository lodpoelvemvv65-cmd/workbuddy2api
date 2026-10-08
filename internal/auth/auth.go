// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// Groups 账号的**业务分组标签**（如 ["internal","external"]），落盘于文件**顶层**
	// groups 键（嵌套形/扁平形同一位置，与 device_token 同风格）。
	//
	// 与 realm 的分工（两者正交，选号时 AND 组合）：
	//   realm —— 账号**固有**的技术域，由 domain 决定、网关强制归一化为 cn/global，
	//            **不可自定义**（见 realmLocked）；
	//   Groups —— **运维自定义**的业务标签，可多个，用来把"哪把密钥能用它"这件事
	//            从技术域里解耦出来。
	//
	// 语义：密钥带 groups 时，只有与之**有交集**的账号参与选号；密钥不带 groups
	// （主密钥 / 未配置）时不过滤。账号 Groups 为空 = **未分组**，默认**不被任何
	// 分组密钥选中**（默认拒绝：宁可漏用，不可串组）。
	//
	// 并发：仅 Parse/LoadDir 赋值一次，其后整个生命周期只读（热加载换的是新的
	// *Auth 指针，而不是改这个字段），故不纳入 a.mu 保护 —— 与 realm 不同
	// （realm 会被 RefreshToken 在锁内改写）。
	Groups []string
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争，go test -race 实证：
//
//	WARNING: DATA RACE
//	Write at ... by goroutine:
//	  (*Client).RefreshToken()  internal/upstream/client.go:929
//	Previous read at ... by goroutine:
//	  (*Client).ChatHeaders()   internal/upstream/headers.go:224
//
// （回归测试 upstream.TestChatHeadersRacesRefreshToken）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
//
// 与 #125 修的 AccessToken/Domain 属同一类：守卫是纯读、刷新是纯写，二者无同步即
// 构成数据竞争（RefreshToken 写回在 client.go「第 2 段（锁内）」的 `if tok.RefreshToken
// != ""` 分支）。go test -race 实证（回归测试 scheduler.TestKeepaliveGuardRacesRefreshToken）：
//
//	WARNING: DATA RACE
//	Read at ... by goroutine:
//	  (*Scheduler).RunKeepaliveNow()  internal/scheduler/scheduler.go:654
//	Previous write at ... by goroutine:
//	  (*Client).RefreshToken()        internal/upstream/client.go:931
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// ExpiresAtValue 加锁读取 access token 的到期时刻（Unix 秒）。RefreshToken 在锁内
// 改写它，故跨包只读也必须经此取值（同 AccessTokenValue/DomainValue/RefreshTokenValue）。
//
// 用途：把「令牌到期」从**控制台读文件的旁路**（console/server.py 直接读 auths/*.json
// 的 expiresAt）搬回网关权威侧 —— 文件值是「上次写回时的快照」，而池内进程持有的
// ExpiresAt 才是选号/续期判定真正用的那个值。两者不一致（如文件陈旧但进程内已续期）
// 时，运维看到的就是一个不存在的「已过期」。
func (a *Auth) ExpiresAtValue() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ExpiresAt
}

// CredWrittenAt 返回凭证文件（FilePath）的最近写回时刻，ok=false 表示文件不可 stat
// （不存在/无权限/FilePath 为空）。
//
// 为什么要它：区分两种「过期显示」——(1) 续期链路正常，文件刚被 SaveAtomic 写过，
// 只是 access token 本身寿命短；(2) 文件长期未更新，说明续期根本没跑（账号被
// disabled 后 RunKeepaliveNow 会 continue 跳过它，见 scheduler.go）——此时「过期」
// 是病因的**症状**，不是原因。只看 expiresAt 一个数字区分不出这两者。
//
// 不返回 error：巡检/状态透出是**尽力而为**的观测，缺文件不该让它失败或报错。
func (a *Auth) CredWrittenAt() (time.Time, bool) {
	if a == nil || a.FilePath == "" {
		return time.Time{}, false
	}
	fi, err := os.Stat(a.FilePath)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// GroupsValue 返回账号分组标签的**副本**（调用方可能缓存或跨 goroutine 传递，
// 返回副本以免误改内部切片）。
func (a *Auth) GroupsValue() []string {
	if a == nil || len(a.Groups) == 0 {
		return nil
	}
	out := make([]string, len(a.Groups))
	copy(out, a.Groups)
	return out
}

// MatchesGroups 报告本账号对「允许分组」是否可见：
//
//	want 为空    → 恒 true（不限分组：主密钥 / 未配置 groups 的密钥语义）
//	账号未打标签 → 恒 false（**默认拒绝**：未分组的账号不给任何分组密钥用）
//	否则         → 要求交集非空
//
// 比较是**精确相等**（不做大小写折叠、不做前缀匹配）：组名规范由密钥侧配置
// （cmd/server/config.go 的正则 fail-fast）与控制台把关，读侧保持"读进来是什么
// 就是什么"，避免两侧规则不一致导致的静默错配。
func (a *Auth) MatchesGroups(want []string) bool {
	if len(want) == 0 {
		return true
	}
	if a == nil || len(a.Groups) == 0 {
		return false
	}
	for _, w := range want {
		for _, g := range a.Groups {
			if w == g {
				return true
			}
		}
	}
	return false
}

// normalizeGroups 归一化账号分组标签：去首尾空白、丢弃空串、按首次出现顺序去重。
//
// 刻意**不做字符集校验**：组名规范由密钥侧配置与控制台把关，读侧保持宽松——否则
// 手写文件里一个尾随空格就会让账号在热加载后静默消失，而热加载是 5 秒一次的常态
// 路径（internal/pool/watch.go），这类静默失配的排查成本极高。
func normalizeGroups(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, g := range in {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用）。
			// 放在 auth 段之外，手写时无需嵌进 auth 对象，降低配置门槛。
			DeviceToken string `json:"device_token"`
			// Groups 顶层 groups（嵌套形与扁平形共用）：账号业务分组标签。
			Groups []string `json:"groups"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
			Groups:       normalizeGroups(n.Groups),
		}
	} else {
		var f struct {
			AccessToken  string   `json:"accessToken"`
			RefreshToken string   `json:"refreshToken"`
			ExpiresAt    int64    `json:"expiresAt"`
			Domain       string   `json:"domain"`
			Realm        string   `json:"realm"`
			UID          string   `json:"uid"`
			EnterpriseID string   `json:"enterpriseId"`
			Nickname     string   `json:"nickname"`
			DeviceToken  string   `json:"device_token"`
			Groups       []string `json:"groups"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
			Groups:       normalizeGroups(f.Groups),
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// Groups 非空才写回顶层 groups（与 device_token 同策略：不给无该字段的旧文件引入空键）。
	//
	// ★ 必须写回 ★ 本函数用**固定字段集整体重写**文件，漏掉哪个键就等于把那个键从磁盘上
	// 删掉。账号的 token 刷新（RefreshToken → SaveAtomic）是高频路径，漏写 groups 会让
	// 分组标签"自己消失"——而且是静默的：标签没了 → 默认拒绝策略下账号被所有分组密钥
	// 忽略 → 表现为"某分组突然无号可用"，但文件里看不到任何报错。
	if len(a.Groups) > 0 {
		doc["groups"] = a.Groups
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// AuthFileGlob auth 文件的统一 glob 模式（宽侧：workbuddy*.json）。
// 网关 LoadDir 与 cmd 运维工具（signin/credit/trial）共用此单一来源——
// 此前 cmd 侧私用 workbuddy-*.json 窄模式，不带连字符的文件（如
// workbuddy_new.json）被网关加载却被运维工具跳过，排障口径对不上
// （审查发现 10）。
const AuthFileGlob = "workbuddy*.json"

// LoadAuthFiles 返回 dir 下按 AuthFileGlob 匹配的 auth 文件清单（已排序）。
// 供 cmd 运维工具复用：只列文件、不解析不迁移（LoadDir 才做 backfill 等副作用），
// 保持 signin/credit/trial 原有的「逐文件 Parse、损坏即跳过/报行错」流程不变。
func LoadAuthFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, AuthFileGlob))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := LoadAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
