// 手动签到入口（POST /v1/checkin）。
//
// 为什么需要它：号池的 credits **只由签到路径写入**（scheduler.CheckinAll →
// SetCreditsDetailed），而自动签到默认排在 9/21 点（外加启动后 5s 的一次首跑）。
// 于是「账号刚入池 / 服务刚重启 / 想立刻确认余额」这三种场景下，控制台看到的是
// 空额度，唯一解法是重启服务 —— 2026-09-22 那次「额度没刷新」就是这个问题。
//
// ★ 必须走网关进程内的 CheckinAll ★ 不能用外部 CLI（deploy/signin）：CLI 是独立
// 进程，签到确实会成功，但网关内存里的 credits 不会更新，控制台照样显示旧值 ——
// 那正是用户最初报的症状。
package server

import (
	"net/http"
	"strconv"
	"time"
)

// checkinCooldown 两次手动签到之间的最小间隔。
//
// 为什么要有：一次签到对每个账号打 2~3 次上游（refresh / daily-checkin / balance），
// global 账号的余额查询还要经过住宅出口代理 —— 被脚本连点会成倍放大上游压力与
// WAF 风险。30s 足够挡住误点与循环脚本，又不妨碍正常使用。
const checkinCooldown = 30 * time.Second

// checkin 处理 POST /v1/checkin：在网关进程内跑一次全量签到并返回报告。
//
// 状态码语义：
//   - 200 报告（含每号 status 与余额；个别账号 fail 仍是 200，看 results）
//   - 429 busy     已有一次签到在跑（scheduler.ErrBusy，手动入口与定时撞车）
//   - 429 cooldown 距上次手动签到不足 checkinCooldown（带 Retry-After）
//   - 501 本实例未接线签到入口（CheckinFn == nil）
func (h *Handler) checkin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CheckinFn == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_wired",
			"本实例未接线签到入口（CheckinFn 为空）")
		return
	}
	now := time.Now()
	if last := h.lastCheckinUnix.Load(); last > 0 {
		if wait := checkinCooldown - now.Sub(time.Unix(last, 0)); wait > 0 {
			secs := strconv.Itoa(int(wait.Seconds()) + 1)
			w.Header().Set("Retry-After", secs)
			writeOpenAIError(w, http.StatusTooManyRequests, "cooldown",
				"刚签到过，请 "+secs+"s 后再试")
			return
		}
	}

	report, busy, err := h.cfg.CheckinFn()
	if busy {
		writeOpenAIError(w, http.StatusTooManyRequests, "busy",
			"签到正在执行中，请稍后再试")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "checkin_failed", err.Error())
		return
	}
	// 盖章放在成功之后：busy / 出错不该占用冷却窗口，否则一次撞车会让用户白等 30s。
	h.lastCheckinUnix.Store(now.Unix())
	if report.Results == nil {
		// 保证 JSON 里是 [] 而不是 null（前端直接 .map 会炸）。
		report.Results = []CheckinResult{}
	}
	writeJSON(w, http.StatusOK, report)
}
