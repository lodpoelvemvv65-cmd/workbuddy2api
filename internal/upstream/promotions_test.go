package upstream

import (
	"testing"
	"time"
)

// promoRaw CN /v3/config 真实样本（2026-09-25 抓取）的裁剪版：含限时免费（截止型）、
// 夜间折扣（daily 时段型）与一条 enabled=false 的干扰项。
const promoRaw = `{"code":0,"data":{"models":[{"id":"hy3"}],"modelPromotions":[
 {"id":"glm-52-night-discount-202607","kind":"discount","enabled":true,"priority":100,
  "modelIds":["glm-5.2"],
  "badge":{"color":"#1E90FF","label":"夜间折扣"},
  "discount":{"discountedCredits":"0.50x","displayMode":"strikethrough","factor":0.5},
  "hover":{"textZh":"每晚 23:00—次日 8:00 积分限时立减，错峰用更省"},
  "schedule":{"daily":[{"end":"7:50","start":"23:00"}],"timezone":"Asia/Shanghai"}},
 {"id":"hy3-free-trial-202608","kind":"discount","enabled":true,"priority":200,
  "modelIds":["hy3","hy3-b","hy3-c"],
  "badge":{"color":"#FF0000","display":"activeOnly","label":"限时免费"},
  "discount":{"discountedCredits":"0x","displayMode":"replace","factor":0},
  "hover":{"textZh":"7月6日–10月31日，每日赠送免费额度。"},
  "schedule":{"timezone":"Asia/Shanghai","validFrom":"2026-07-06T00:00:00+08:00","validUntil":"2026-11-01T00:00:00+08:00"}},
 {"id":"disabled-promo","kind":"discount","enabled":false,"modelIds":["x"],"badge":{"label":"不该出现"}}
]}}`

// TestParseModelPromotions 解析全字段 + enabled=false 剔除 + 非对象形态返回 nil。
func TestParseModelPromotions(t *testing.T) {
	promos := parseModelPromotions([]byte(promoRaw))
	if len(promos) != 2 {
		t.Fatalf("promos=%d want 2 (disabled filtered)", len(promos))
	}
	night := promos[0]
	if night.ID != "glm-52-night-discount-202607" || night.BadgeLabel != "夜间折扣" || night.BadgeColor != "#1E90FF" {
		t.Errorf("night promo parsed wrong: %+v", night)
	}
	if !night.HasDiscount || night.Factor != 0.5 || night.DiscountedCredits != "0.50x" {
		t.Errorf("night discount parsed wrong: %+v", night)
	}
	if len(night.Daily) != 1 || night.Daily[0].Start != "23:00" || night.Daily[0].End != "7:50" {
		t.Errorf("night daily window parsed wrong: %+v", night.Daily)
	}
	if night.ValidUntil != "" {
		t.Errorf("night promo should have no validUntil, got %q", night.ValidUntil)
	}

	free := promos[1]
	if free.BadgeLabel != "限时免费" || free.BadgeDisplay != "activeOnly" {
		t.Errorf("free promo badge parsed wrong: %+v", free)
	}
	if !free.HasDiscount || free.Factor != 0 || free.DiscountedCredits != "0x" {
		t.Errorf("free promo discount parsed wrong: %+v", free)
	}
	if free.ValidFrom != "2026-07-06T00:00:00+08:00" || free.ValidUntil != "2026-11-01T00:00:00+08:00" {
		t.Errorf("free promo schedule parsed wrong: from=%q until=%q", free.ValidFrom, free.ValidUntil)
	}
	if free.HoverTextZh != "7月6日–10月31日，每日赠送免费额度。" {
		t.Errorf("free promo hover text wrong: %q", free.HoverTextZh)
	}
	if !free.AppliesTo("hy3-b") || free.AppliesTo("nope") {
		t.Errorf("AppliesTo wrong: %v", free.ModelIDs)
	}

	// 窄表形态（data 为数组）→ 无 modelPromotions → nil。
	if got := parseModelPromotions([]byte(`{"code":0,"data":["gpt-5.4"]}`)); got != nil {
		t.Errorf("narrow table should yield nil, got %v", got)
	}
	// 无该字段 → nil。
	if got := parseModelPromotions([]byte(`{"code":0,"data":{"models":[{"id":"hy3"}]}}`)); got != nil {
		t.Errorf("no modelPromotions should yield nil, got %v", got)
	}
}

// TestModelPromotionActiveAt 生效判定：validUntil 未来/过去、daily 时段含跨午夜。
func TestModelPromotionActiveAt(t *testing.T) {
	// 2026-09-25 12:00 +08:00。
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, cstShanghai)

	free := ModelPromotion{
		Enabled: true, Timezone: "Asia/Shanghai",
		ValidFrom: "2026-07-06T00:00:00+08:00", ValidUntil: "2026-11-01T00:00:00+08:00",
	}
	if !free.ActiveAt(now) {
		t.Error("free promo should be active within valid window")
	}
	expired := ModelPromotion{Enabled: true, ValidUntil: "2026-09-01T00:00:00+08:00"}
	if expired.ActiveAt(now) {
		t.Error("expired promo must not be active")
	}
	future := ModelPromotion{Enabled: true, ValidFrom: "2026-10-01T00:00:00+08:00"}
	if future.ActiveAt(now) {
		t.Error("not-yet-started promo must not be active")
	}
	disabled := ModelPromotion{Enabled: false}
	if disabled.ActiveAt(now) {
		t.Error("disabled promo must not be active")
	}

	// 夜间折扣 23:00–7:50（跨午夜）：12:00 不生效，02:00 生效，23:30 生效。
	night := ModelPromotion{
		Enabled: true, Timezone: "Asia/Shanghai",
		Daily: []PromotionWindow{{Start: "23:00", End: "7:50"}},
	}
	if night.ActiveAt(now) {
		t.Error("night promo must be inactive at 12:00")
	}
	if !night.ActiveAt(time.Date(2026, 9, 25, 2, 0, 0, 0, cstShanghai)) {
		t.Error("night promo must be active at 02:00")
	}
	if !night.ActiveAt(time.Date(2026, 9, 25, 23, 30, 0, 0, cstShanghai)) {
		t.Error("night promo must be active at 23:30")
	}
}

// TestPromotionsCacheRealmIsolation store/snapshot 按 realm 分层，且空列表不清缓存。
func TestPromotionsCacheRealmIsolation(t *testing.T) {
	c := &Client{}
	cn := []ModelPromotion{{ID: "cn-promo", BadgeLabel: "限时免费", Enabled: true}}
	gl := []ModelPromotion{{ID: "gl-promo", BadgeLabel: "Free now", Enabled: true}}
	c.storePromotions("cn", cn)
	c.storePromotions("global", gl)

	if got := c.PromotionsSnapshot("cn"); len(got) != 1 || got[0].ID != "cn-promo" {
		t.Errorf("cn snapshot=%v", got)
	}
	if got := c.PromotionsSnapshot("global"); len(got) != 1 || got[0].ID != "gl-promo" {
		t.Errorf("global snapshot=%v", got)
	}
	// 空列表不覆盖既有缓存。
	c.storePromotions("cn", nil)
	if got := c.PromotionsSnapshot("cn"); len(got) != 1 {
		t.Errorf("empty store must not clear cache, got %v", got)
	}
	// nil client 安全。
	var nilC *Client
	if got := nilC.PromotionsSnapshot("cn"); got != nil {
		t.Errorf("nil client snapshot=%v want nil", got)
	}
}

// TestMergePromotions v3 为主、企业端点只补缺失 id。
func TestMergePromotions(t *testing.T) {
	primary := []ModelPromotion{{ID: "a", BadgeLabel: "A"}, {ID: "b", BadgeLabel: "B"}}
	secondary := []ModelPromotion{{ID: "b", BadgeLabel: "B2"}, {ID: "c", BadgeLabel: "C"}}
	got := mergePromotions(primary, secondary)
	if len(got) != 3 || got[0].ID != "a" || got[1].ID != "b" || got[1].BadgeLabel != "B" || got[2].ID != "c" {
		t.Errorf("mergePromotions=%+v want [a b(B) c]", got)
	}
}
