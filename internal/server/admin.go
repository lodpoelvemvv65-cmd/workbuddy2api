// admin.go 运维管理端点（issue #138 / #118）：账号的临时停用 / 恢复 / 复活。
//
// 设计要点（与维护者在 issue #118 预告的方案一致）：
//   - 手动停用是**独立状态位** manual_disabled，与自动禁用 disabled 并列、互不影响。
//     复用同一字段会让运维意图被签到解冻、refresh 成功等自动复活路径意外解除。
//   - 语义是「对话流量摘除」而非「账号冻结」：不碰冷却/熔断维度，签到与保活照常，
//     凭证和积分都是活的；恢复时拿到的是停用期间真实发生的状态。
//   - 默认关闭（config admin.enabled），开启后与 /status 共用同一把 api_key 鉴权。
//   - 幂等：面板重试不会报错；重复调用只更新原因文案。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/logfmt"
)

// adminKeyAllowed 判定本次请求是否有权使用管理面（P0-1 实施时补的提权闸）。
//
// ★ 为什么必须有它 ★
// withAuth 只校验「密钥在生效表里」，而生效表 = 主密钥 + config.api_keys 里的
// **分组密钥**（AuthKey.Groups 非空）。分组密钥是发给外部调用方的**受限模型调用
// 凭证**（选号时按 groups AND 过滤账号）。若不额外收紧，一旦 open
// admin.enabled，外部调用方手里那把带分组的密钥就能调
// /admin/accounts/{uid}/disable 把整个号池摘空——这是纯粹的权限升级，
// 且是"开了开关才出现"的、最容易漏测的一类。
//
// 判据用「Groups 为空」而不是「== 主密钥」：
//   - 主密钥与"不带分组的密钥"在既有语义下**本就等价**（都不过滤账号），
//     复用同一判据不会引入第二套"谁是管理员"的定义，见 authKeyFrom 的注释
//     （nil = 不限分组）；
//   - 主密钥轮换期若在 config.api_keys 里并存新旧两把，两把都自动获得管理权，
//     不需要改代码（与"新老密钥并存"的密钥轮换方向一致）。
//
// 不鉴权部署（密钥表为空 → authKeyFrom 返回 nil）放行：此时"分组密钥"概念不存在，
// 管理面本身又是显式 opt-in（admin.enabled 缺省 false），放行不构成提权。
func adminKeyAllowed(r *http.Request) bool {
	k := authKeyFrom(r.Context())
	return k == nil || len(k.Groups) == 0
}

// withOps 在 withAuth 之内、audit 之外再加一层"必须是运维密钥"的闸。
// 拒绝时回 403 并**直接返回**（不落审计）：管理操作审计的对象是"运维做了什么"，
// 把外部用户未授权的探测也记进去，等于给了他们一个往运维磁盘写内容的入口。
func withOps(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !adminKeyAllowed(r) {
			writeOpenAIError(w, http.StatusForbidden, "forbidden",
				"this endpoint requires an unrestricted (non-group) API key")
			return
		}
		next(w, r)
	}
}

// adminRefreshResult 「立即续期」端点的响应：把"这次续期到底成没成、现在的到期
// 时刻是多少"一次说清，面板据此直接刷新那一行，不必再打一次 /status。
type adminRefreshResult struct {
	UID string `json:"uid"`
	OK  bool   `json:"ok"`
	// Detail 结果说明：失败时是错误摘要（已截断），成功时留空。
	Detail string `json:"detail,omitempty"`
	// TokenExpiresAt 续期成功后 access token 的新到期时刻（Unix 秒）。
	TokenExpiresAt int64 `json:"token_expires_at,omitempty"`
	// NeedsRelogin 该号只能靠重新登录恢复（无 refresh token / refresh token 已失效）。
	// 面板据此把按钮从「立即续期」切成「重新登录」，而不是让运维反复点一个必然失败的按钮。
	NeedsRelogin bool `json:"needs_relogin"`
}

// adminState 管理端点的统一响应体：回显操作后的双位状态，面板据此直接更新 UI，
// 不必再打一次 /status。
type adminState struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason,omitempty"`
	// Disabled 保留在响应里让面板能区分「手动摘除」与「系统判定坏了」——
	// 恢复按钮的语义对两者不同（enable 解手动位，revive 解自动位）。
	Disabled bool `json:"disabled"`
	Changed  bool `json:"changed"`
}

// adminUID 提取并校验路径段 uid。返回 false 表示已写出响应（uid 为空 → 400），
// 调用方应直接 return。
// 开关判断不在这里：路由按 cfg.AdminEnabled 条件注册（见 NewHandler），
// 未开启时这些 handler 根本不可达——handler 内再判开关是多余的存在性泄露面。
func adminUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return "", false
	}
	return uid, true
}

// adminReasonFromBody 读可选 JSON 体里的 reason 字段。
// 空体/非 JSON/无该字段都返回空串（端点不因体格式拒绝——无体是最常见调用形态）。
func adminReasonFromBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	// 限制读取量：reason 是短文本，避免畸形大请求占用内存。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

// adminAccountDisable 手动停用：把账号摘出选号池，但保留在池里
// （状态/冷却/成本台账继续归它管，签到与保活照常）。
func (h *Handler) adminAccountDisable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	reason := adminReasonFromBody(r)
	if reason == "" {
		reason = "manual"
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, true, reason)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, stopReason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: stopReason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountEnable 解除手动停用。若账号仍被系统自动禁用（disabled），它**不会**
// 因此回到选号池——那需要 revive。响应里的 disabled 字段就是给面板看的提示。
func (h *Handler) adminAccountEnable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, false, "")
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountRevive 解除系统自动禁用（清 disabled + reason + 连续 12153 计数）。
// 不碰手动停用位：运维明确摘除的号不应被一次 revive 悄悄放回选号池。
func (h *Handler) adminAccountRevive(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	if _, _, found := h.cfg.Pool.ManualDisabledState(uid); !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	changed := h.cfg.Pool.ReviveDisabled(uid)
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// accountAutoDisabled 读某账号当前的自动禁用位（供响应回显）。
// 复用 Pool.List 的单账号查询：轮询全部账号在小池下开销可忽略，
// 且避免为此在 pool 上再开一个只读访问器（保持接口面最小）。
func (h *Handler) accountAutoDisabled(uid string) bool {
	for _, st := range h.cfg.Pool.List() {
		if st.UID == uid {
			return st.Disabled
		}
	}
	return false
}

// adminAccountRefresh 「立即续期」（P0-1）：对单个账号显式跑一次 token 续期。
//
// ★ 为什么现有自动路径不够 ★
// RunKeepaliveNow 的第一条语句是 `if st.Disabled { continue }` —— 账号一旦被禁用，
// 每日 22:00 的保活**不再碰它**。于是 auth 文件里的 expiresAt 永远停在旧值，
// 控制台的「令牌到期」列长期显示"已过期"，看着就像"号已经废了"。而 disabled 恰恰
// 是最需要补一次续期来**验证**它到底死没死的场景（历史 P0-1 侦察：13 个 disabled
// 号全部能 refresh 成功，是误判的受害者）。本端点把这条验证路径从"停服 + 手工改
// state.json"（还得当心被 flusher 覆盖写回）降级为一次带鉴权的 HTTP 调用。
//
// ★ 边界刻意收窄，与 disable/enable/revive 互补而不重叠 ★
//   - 不换号、不重试：单次尝试、结果如实回传。重试是 chat 路径的策略，不是
//     运维"验证凭证是否可用"的语义——重试会把"这次失败"掩盖成"下次也许行"。
//   - 不动 disabled / manual_disabled：本端点只**证明凭证还活着**；要不要把号
//     放回选号池是 revive 的职责。两件事分开，运维才看得清因果链。
//   - 失败**不**喂 NoteError / 熔断：验证性调用不该因为"上游此刻抖了一下"
//     给账号记一笔账、甚至推进熔断退避。只有成功才回写状态（且都是正向的）。
//   - 成功时清 12153 误判计数（ClearSessionDead）：与 keepalive 成功同口径——
//     一次成功的续期就是"session 没死"的实证。
func (h *Handler) adminAccountRefresh(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	// 前置守卫：没有 refresh token 的号，续期**必然**失败（上游拿什么去换？）。
	// 直接回 409 并把台账记上——省一次注定失败的上游往返，也让巡检与手动操作
	// 看到同一个判据（needs_relogin=true）。
	if a.RefreshTokenValue() == "" {
		h.cfg.Pool.NoteRefreshFail(uid, errors.New("no refresh token"))
		writeJSON(w, http.StatusConflict, adminRefreshResult{
			UID: uid, OK: false, NeedsRelogin: true,
			Detail: "no refresh token — must re-login (OAuth device flow) to obtain a new one",
		})
		return
	}
	if err := h.cfg.Upstream.RefreshToken(a); err != nil {
		// 只记观测台账，不判罚（见函数注释）。是"该杀号"还是"该重登"，由运维
		// 结合 /status 的 needs_relogin / session_dead_fails 决定——本端点不替
		// 他做不可逆决定。
		h.cfg.Pool.NoteRefreshFail(uid, err)
		writeJSON(w, http.StatusBadGateway, adminRefreshResult{
			UID: uid, OK: false, Detail: logfmt.Truncate(err.Error(), 200),
		})
		return
	}
	h.cfg.Pool.ClearSessionDead(uid) // 成功 = session 未死的实证（同 keepalive 口径）
	h.cfg.Pool.NoteRefreshOK(uid)
	a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
	if err := a.SaveAtomic(); err != nil {
		// 上游续期成功、内存里 token 已是新的，但落盘失败 → 重启即回滚到旧 token。
		// 与 chat/keepalive 路径同口径必须暴露，且**不能**回 200：那会让运维以为
		// 已经安全了，而实际上"过两天重启就白干"。
		log.Printf("ERR: [admin] refresh acct=%s: save auth failed: %v",
			logfmt.Label(uid, a.Nickname), err)
		writeJSON(w, http.StatusInternalServerError, adminRefreshResult{
			UID: uid, OK: false,
			Detail:         "refreshed in memory but persist failed (will revert on restart): " + logfmt.Truncate(err.Error(), 160),
			TokenExpiresAt: a.ExpiresAtValue(),
		})
		return
	}
	writeJSON(w, http.StatusOK, adminRefreshResult{
		UID: uid, OK: true, TokenExpiresAt: a.ExpiresAtValue(),
	})
}
