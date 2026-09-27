// promotions.go 上游模型促销/折扣活动解析（/v3/config 与 global 企业端点的
// data.modelPromotions[]）。
//
// 背景：上游「限时免费 / 夜间免费 / 夜间折扣」的标签与截止时间**不在**模型对象
// 的 tags 里（企业端点 tags 里那个 `badge:限时免费:#FF0000` 只有 label:color，
// 没有截止时间），而是挂在独立的 modelPromotions 活动对象上：badge（标签+颜色）+
// discount（折扣倍率）+ schedule.validFrom/validUntil（截止时间）或 schedule.daily
// （每日时段）。本包只做**展示**用解析——促销一律不参与选号/成本分层。
package upstream

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// PromotionWindow 促销的每日生效时段（schedule.daily 单条，HH:MM 墙钟，
// 按 ModelPromotion.Timezone 解释；start > end 表示跨午夜）。
type PromotionWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// ModelPromotion 上游模型促销/折扣活动（data.modelPromotions[] 单条）。
// 仅用于展示（/v1/models 条目 + 面板），不参与选号。
type ModelPromotion struct {
	ID                string            // 活动 id（如 hy3-free-trial-202608）
	Kind              string            // 活动类型（如 discount）
	Enabled           bool              // 上游 enabled（缺省视为 true）
	Priority          int               // 同模型多活动时的优先级（大者优先）
	ModelIDs          []string          // 作用到的模型 id 列表
	BadgeLabel        string            // 标签文案（限时免费 / Free now / 夜间折扣）
	BadgeColor        string            // 标签颜色（#FF0000）
	BadgeDisplay      string            // 展示模式（activeOnly = 仅生效时显示）
	HasDiscount       bool              // 是否带 discount 块（无 discount 的 badge 占位活动为 false）
	Factor            float64           // 折扣系数（0 = 免费）
	DiscountedCredits string            // 折后倍率原文（如 "0x"）
	DisplayMode       string            // 折扣展示方式（replace / strikethrough）
	HoverTextZh       string            // 悬浮说明（含人类可读日期区间）
	Timezone          string            // 时段解释时区（Asia/Shanghai）
	ValidFrom         string            // 活动开始（RFC3339）
	ValidUntil        string            // 活动截止（RFC3339）
	Daily             []PromotionWindow // 每日生效时段（与 validFrom/Until 二选一或并存）
}

// promotionJSON modelPromotions[] 单条的上游 JSON 形态（解析用）。
type promotionJSON struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Enabled  *bool    `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    struct {
		Label   string `json:"label"`
		Color   string `json:"color"`
		Display string `json:"display"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		DisplayMode       string  `json:"displayMode"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	Hover struct {
		TextZh string `json:"textZh"`
		TextEn string `json:"textEn"`
	} `json:"hover"`
	Schedule struct {
		Timezone   string            `json:"timezone"`
		ValidFrom  string            `json:"validFrom"`
		ValidUntil string            `json:"validUntil"`
		Daily      []PromotionWindow `json:"daily"`
	} `json:"schedule"`
}

// parseModelPromotions 从上游模型目录原始响应中解析 data.modelPromotions[]。
// 非对象形态（窄表 data 为数组）/ 无该字段 / 解析失败 → nil（不编造）。
// 上游 enabled=false 的活动直接剔除（只透出生效候选，是否"当前生效"由
// ModelPromotion.ActiveAt 按 validFrom/Until + daily 时段另行判定）。
func parseModelPromotions(raw []byte) []ModelPromotion {
	var env struct {
		Data struct {
			ModelPromotions []promotionJSON `json:"modelPromotions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	if len(env.Data.ModelPromotions) == 0 {
		return nil
	}
	out := make([]ModelPromotion, 0, len(env.Data.ModelPromotions))
	for _, p := range env.Data.ModelPromotions {
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		if !enabled {
			continue
		}
		mp := ModelPromotion{
			ID:           p.ID,
			Kind:         p.Kind,
			Enabled:      enabled,
			Priority:     p.Priority,
			ModelIDs:     p.ModelIDs,
			BadgeLabel:   p.Badge.Label,
			BadgeColor:   p.Badge.Color,
			BadgeDisplay: p.Badge.Display,
			HoverTextZh:  p.Hover.TextZh,
			Timezone:     p.Schedule.Timezone,
			ValidFrom:    p.Schedule.ValidFrom,
			ValidUntil:   p.Schedule.ValidUntil,
			Daily:        p.Schedule.Daily,
		}
		if p.Discount != nil {
			mp.HasDiscount = true
			mp.Factor = p.Discount.Factor
			mp.DiscountedCredits = p.Discount.DiscountedCredits
			mp.DisplayMode = p.Discount.DisplayMode
		}
		out = append(out, mp)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AppliesTo 报告该活动是否作用于指定模型 id。
func (p ModelPromotion) AppliesTo(modelID string) bool {
	for _, id := range p.ModelIDs {
		if id == modelID {
			return true
		}
	}
	return false
}

// ActiveAt 报告该活动在 now 是否生效：
//   - enabled；
//   - 落在 validFrom..validUntil 窗口内（二者任一缺省则视为该侧不限制）；
//   - 若声明了 daily 时段，当前墙钟（按 Timezone 解释）落在任一时段内。
//
// 时区优先 Asia/Shanghai 固定 +8（与上游官网展示时区一致，不依赖容器 tzdata），
// 其它时区走 LoadLocation，失败回落本地时区。
func (p ModelPromotion) ActiveAt(now time.Time) bool {
	if !p.Enabled {
		return false
	}
	n := now.In(p.location())
	if p.ValidFrom != "" {
		if t, err := time.Parse(time.RFC3339, p.ValidFrom); err == nil && n.Before(t) {
			return false
		}
	}
	if p.ValidUntil != "" {
		if t, err := time.Parse(time.RFC3339, p.ValidUntil); err == nil && !n.Before(t) {
			return false
		}
	}
	if len(p.Daily) > 0 && !inDailyWindow(n, p.Daily) {
		return false
	}
	return true
}

// location 解析活动时区：Asia/Shanghai 家族走固定 +8（cstShanghai，见 growth_bonus.go，
// 不依赖容器 tzdata）；其它时区 LoadLocation；空/失败回落本地时区。
func (p ModelPromotion) location() *time.Location {
	switch strings.TrimSpace(p.Timezone) {
	case "Asia/Shanghai", "CST", "UTC+8":
		return cstShanghai
	case "":
		return time.Local
	}
	if loc, err := time.LoadLocation(p.Timezone); err == nil {
		return loc
	}
	return time.Local
}

// inDailyWindow 判定墙钟分钟数是否落在任一时段内；start > end 视为跨午夜
// （如 23:00–7:50）。
func inDailyWindow(n time.Time, windows []PromotionWindow) bool {
	cur := n.Hour()*60 + n.Minute()
	for _, w := range windows {
		s, ok1 := parseHHMM(w.Start)
		e, ok2 := parseHHMM(w.End)
		if !ok1 || !ok2 {
			continue
		}
		if s <= e {
			if cur >= s && cur < e {
				return true
			}
		} else if cur >= s || cur < e {
			return true
		}
	}
	return false
}

// parseHHMM 把 "HH:MM" 解析成当日分钟数；非法返回 ok=false。
func parseHHMM(s string) (int, bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// storePromotions 按 realm 写入促销活动缓存（cn/global 分层，避免两域同 id 活动串味）。
// 空列表跳过写（上游某路失败/窄表时不得清掉既有活动）。
func (c *Client) storePromotions(realm string, promos []ModelPromotion) {
	if c == nil || len(promos) == 0 {
		return
	}
	c.promotionsMu.Lock()
	defer c.promotionsMu.Unlock()
	if c.promotions == nil {
		c.promotions = make(map[string][]ModelPromotion)
	}
	c.promotions[realmKey(realm)] = promos
}

// PromotionsSnapshot 返回指定 realm 的促销活动只读副本（未探测/缓存冷 → nil）。
// 只读已有数据，不发起上游请求（服务 /v1/models 展示，与 GlobalModelInfosSnapshot 同口径）。
func (c *Client) PromotionsSnapshot(realm string) []ModelPromotion {
	if c == nil {
		return nil
	}
	c.promotionsMu.RLock()
	defer c.promotionsMu.RUnlock()
	p := c.promotions[realmKey(realm)]
	if len(p) == 0 {
		return nil
	}
	out := make([]ModelPromotion, len(p))
	copy(out, p)
	return out
}

// mergePromotions 合并两路促销活动：primary（v3）为主，secondary（企业端点）只补
// primary 缺失的活动 id。去重 key = 活动 id；无 id 的活动不去重（原样保留）。
func mergePromotions(primary, secondary []ModelPromotion) []ModelPromotion {
	if len(secondary) == 0 {
		return primary
	}
	seen := make(map[string]bool, len(primary))
	out := make([]ModelPromotion, 0, len(primary)+len(secondary))
	for _, p := range primary {
		if p.ID != "" && seen[p.ID] {
			continue
		}
		if p.ID != "" {
			seen[p.ID] = true
		}
		out = append(out, p)
	}
	for _, p := range secondary {
		if p.ID != "" && seen[p.ID] {
			continue
		}
		if p.ID != "" {
			seen[p.ID] = true
		}
		out = append(out, p)
	}
	return out
}
