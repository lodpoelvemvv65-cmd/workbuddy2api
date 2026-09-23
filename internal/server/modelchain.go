// 模型候选链：把客户端写的模型名展开成一组可依次尝试的出站候选。
//
// 为什么需要它：上游把「同一个模型」拆成了多个独立记账的 id——分域（cn: / global:）、
// 分区域变体（-sg）、分计费档（x0.00 免费 / x0.03 积分）。客户端（Claude Code 等）
// 只知道模型名，不该被迫记住这些后缀，更不该在免费额度用尽时手动改配置。
//
// 归一化规则（只作用于客户端入参，出站一律用候选自身的 id）：
//
//  1. 剥 realm 前缀：cn: / global: 是网关路由协议，不参与「同名家族」判定；
//  2. 剥区域后缀：-sg 这类上游区域变体与本体同族（实测 global 有
//     deepseek-v4.1-flash-sg，与 deepseek-v4.1-flash 同价档不同 id）；
//  3. 上下文标记 [1m]/[200k]/[1000000]：解析成最小上下文档位需求，不是模型名的一部分
//     （上游不认该后缀，直传得 503 no such model）。
//
// 候选排序（决定「先用谁」）：免费（x0.00）→ 未知倍率 → 倍率升序；
// 同价时客户端显式写了 realm 前缀的那个域优先（显式意图优先，但不做硬过滤——
// 硬过滤会让"免费档用完自动换积分档"失效）。
package server

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"workbuddy2api/internal/upstream"
)

// regionSuffix 上游区域变体后缀。实测 global 的 deepseek-v4.1-flash-sg 与
// deepseek-v4.1-flash 同族同能力，只是 id 多一个后缀；客户端不该被迫记住它。
const regionSuffix = "-sg"

// aliasModel 按 config 的 model_aliases 把客户端惯用名换成真实上游模型名。
// 第二个返回值报告是否命中（供日志记实际用的名字）。
//
// 匹配键用 canonicalModelName（剥 realm 前缀 / 上下文标记 / 区域后缀、小写折叠），
// 所以配置里写 "DeepSeek-Flash" 或 "deepseek-flash[1M]" 都能命中同一条规则。
// 未命中 / 空表 → 原样返回，行为零变化。
func (h *Handler) aliasModel(model string) (string, bool) {
	if model == "" || len(h.cfg.ModelAliases) == 0 {
		return model, false
	}
	target, ok := h.cfg.ModelAliases[canonicalModelName(model)]
	if !ok || target == "" || target == model {
		return model, false
	}
	return rewriteAliasKeepingAffixes(model, target), true
}

// rewriteAliasKeepingAffixes 换别名时只替换模型主体，保留客户端写的前缀与上下文标记：
//
//	"global:ds-flash[1M]" + target "deepseek-v4.1-flash" -> "global:deepseek-v4.1-flash[1M]"
//
// 而目标自身带 realm 前缀（配置写 "global:gpt-5.6-luna"）时以目标为准，不再叠加——
// 与 resolveAnthropicModel 的默认模型前缀口径一致。
func rewriteAliasKeepingAffixes(model, target string) string {
	marker := ""
	if m := contextMarkerRe.FindStringSubmatch(model); m != nil {
		marker = model[len(m[1]):] // m[1] 是标记前的主体（可能含 realm 前缀）
	}
	if strings.Contains(target, ":") {
		return target + marker
	}
	if idx := strings.IndexByte(model, ':'); idx >= 0 {
		if p := model[:idx]; p == "cn" || p == "global" {
			return p + ":" + target + marker
		}
	}
	return target + marker
}

// modelCandidate 一次出站可用的候选模型。
type modelCandidate struct {
	Realm string // "cn" / "global"
	ID    string // 上游裸模型名（直接写进出站 body 的 model 字段）
	ctx   int64  // 上下文窗口（用于 [1M] 过滤）
	cost  float64
	known bool // 倍率是否已知（Credits 原文可解析）
}

// parseContextMarker 解析模型名尾部的上下文标记，返回净名与最小上下文需求。
//
//	"deepseek-flash[1m]"     -> ("deepseek-flash", 1000000)
//	"deepseek-flash[200k]"   -> ("deepseek-flash", 200000)
//	"deepseek-flash[1000000]"} -> ("deepseek-flash", 1000000)
//	"deepseek-flash"         -> ("deepseek-flash", 0)   无需求
//
// 复用 anthropic.go 的 contextMarkerRe（同一条标记语法，两处口径必须一致）。
// 数字解析失败或无单位歧义（如 "[0m]"）时退回无需求，不臆造档位。
func parseContextMarker(model string) (string, int64) {
	m := contextMarkerRe.FindStringSubmatch(model)
	if m == nil {
		return model, 0
	}
	// 标记一律剥掉（与 stripContextMarker 同口径：标记永远不是模型名的一部分），
	// 只有档位值合法且为正才产生「最小上下文需求」。非数字标记（[abc]）与纯标记
	// （"[1m]"，被正则的 (.+) 保护）都不匹配，原样返回。
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || n <= 0 {
		return m[1], 0
	}
	unit := int64(1)
	switch strings.ToLower(m[3]) {
	case "k":
		unit = 1000
	case "m":
		unit = 1000000
	}
	if n > math.MaxInt64/unit {
		return m[1], 0 // 溢出：退回「无需求」，不臆造一个天文数字档位
	}
	return m[1], n * unit
}

// canonicalModelName 归一化到「同名家族」键：剥 realm 前缀 + 上下文标记 + 区域后缀，
// 最后小写折叠（上游 id 全小写；客户端大小写差异不该造成漏匹配）。
// 仅用于候选匹配，绝不用于出站——出站走候选自身的 ID。
func canonicalModelName(model string) string {
	_, bare := resolveModel(model)
	bare, _ = parseContextMarker(bare)
	bare = strings.ToLower(strings.TrimSpace(bare))
	return strings.TrimSuffix(bare, regionSuffix)
}

// parseCredits 解析上游倍率原文为数值。
// 上游格式不统一："x0.05 credits" / "x0.05" / "0.05" 都是同一口径（见 fmtCreditsPrefix）。
// 第二个返回值区分「明确是 0（免费）」与「没有倍率信息（未知）」——两者排序不同，
// 不能都折成 0。
func parseCredits(raw string) (float64, bool) {
	s := strings.TrimSpace(strings.ToLower(raw))
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "x"))
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return f, true
}

// modelChain 把客户端模型名展开成出站候选链（免费优先 → 积分档）。
//
// prefRealm 是客户端显式写的 realm 前缀（resolveModel 结果，裸名恒为 "cn"），
// 只作为同价时的排序偏好，不做硬过滤：跨域兜底正是「不管国内国外，免费用完用积分」
// 的落点。
//
// 目录里没有同名模型 → 单元素链（原样透传，绝不臆造候选）：未知模型名的行为与
// 引入本链之前逐字一致，客户端写什么都没被网关改写。
func (h *Handler) modelChain(clientModel, prefRealm, bareModel string) []modelCandidate {
	canon := canonicalModelName(clientModel)
	if canon == "" {
		return nil
	}
	want, needCtx := parseContextMarker(bareModel)

	var cands []modelCandidate
	seen := map[string]bool{}
	add := func(realm string, mi upstream.ModelInfo) {
		id := mi.ID
		if id == "" || seen[realm+"|"+id] || canonicalModelName(id) != canon {
			return
		}
		seen[realm+"|"+id] = true
		c, ok := parseCredits(mi.Credits)
		cands = append(cands, modelCandidate{
			Realm: realm,
			ID:    id,
			ctx:   upstream.ContextWindowListingV4(id, mi.ContextWindow, h.cfg.Upstream.HTTP),
			cost:  c,
			known: ok,
		})
	}
	// 只读快照，不在 chat 热路径上发起上游探测：多一次上游往返就是实打实的延迟 +
	// WAF 记账放大（与 wafip.go 同哲学）。两张目录由 /v1/models 或后台
	// WarmModelCatalog 预热（见 handler.go 注释）；冷 / 过期时快照为空 →
	// 退化成单候选透传，行为与引入本链之前逐字一致，绝不阻塞请求。
	if catalogCold() {
		// 快照冷：本次请求按单候选透传走（不等目录），顺手在后台补一次预热，
		// 下一个请求即恢复完整候选链。见 triggerCatalogWarm 注释。
		h.triggerCatalogWarm()
	}
	for _, mi := range dynamicModelsSnapshot() {
		add("cn", mi)
	}
	if h.cfg.GlobalEnabled {
		for _, mi := range h.cfg.Upstream.GlobalModelInfosSnapshot() {
			add("global", mi)
		}
	}
	if len(cands) == 0 {
		return []modelCandidate{{Realm: prefRealm, ID: want}}
	}

	// [1M] 过滤：有满足档位的候选就只留它们；一个都没有 → 退回全部候选
	// （上游没有 1M 版本时就按原来的模型走，不退化成报错）。
	if needCtx > 0 {
		fit := make([]modelCandidate, 0, len(cands))
		for _, c := range cands {
			if c.ctx >= needCtx {
				fit = append(fit, c)
			}
		}
		if len(fit) > 0 {
			cands = fit
		}
	}

	// 免费优先 → 未知 → 倍率升序；同价时显式前缀域优先。
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.known != b.known {
			return a.known
		}
		if a.known && a.cost != b.cost {
			return a.cost < b.cost
		}
		ap, bp := a.Realm == prefRealm, b.Realm == prefRealm
		if ap != bp {
			return ap
		}
		return false
	})
	return cands
}
