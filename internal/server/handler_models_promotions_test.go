package server

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// promotionsModelsBody CN /v3/config 样本：模型目录 + 限时免费促销（modelPromotions）。
// validUntil 取远期（2099）保证 active 与运行日期无关。
const promotionsModelsBody = `{"code":0,"data":{
 "models":[{"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"tags":["craft"]}],
 "agents":[{"name":"cli","models":["hy3"]}],
 "modelPromotions":[
  {"id":"hy3-free-trial-202608","kind":"discount","enabled":true,"priority":200,
   "modelIds":["hy3","hy3-b"],
   "badge":{"color":"#FF0000","display":"activeOnly","label":"限时免费"},
   "discount":{"discountedCredits":"0x","displayMode":"replace","factor":0},
   "hover":{"textZh":"每日赠送免费额度。"},
   "schedule":{"timezone":"Asia/Shanghai","validFrom":"2026-07-06T00:00:00+08:00","validUntil":"2099-11-01T00:00:00+08:00"}},
  {"id":"other-model-promo","kind":"discount","enabled":true,"modelIds":["unrelated"],
   "badge":{"color":"#1E90FF","label":"夜间折扣"},
   "schedule":{"daily":[{"start":"23:00","end":"7:50"}],"timezone":"Asia/Shanghai"}}
 ]}}`

// TestModelListPromotionsDynamicCN /v1/models 条目透出促销活动（label/color/factor/
// valid_until/active），且只命中 modelIds 覆盖该模型的活动（unrelated 活动不透出）。
func TestModelListPromotionsDynamicCN(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, promotionsModelsBody, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	var entry map[string]any
	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok && id == "cn:hy3" {
			entry = m
			break
		}
	}
	if entry == nil {
		t.Fatal("cn:hy3 not found in modelList output")
	}
	raw, ok := entry["promotions"].([]map[string]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("promotions=%v want 1 entry (only hy3-free-trial)", entry["promotions"])
	}
	promo := raw[0]
	if promo["label"] != "限时免费" {
		t.Errorf("label=%v want 限时免费", promo["label"])
	}
	if promo["color"] != "#FF0000" {
		t.Errorf("color=%v want #FF0000", promo["color"])
	}
	if promo["kind"] != "discount" {
		t.Errorf("kind=%v want discount", promo["kind"])
	}
	if promo["factor"] != float64(0) {
		t.Errorf("factor=%v want 0", promo["factor"])
	}
	if promo["valid_until"] != "2099-11-01T00:00:00+08:00" {
		t.Errorf("valid_until=%v", promo["valid_until"])
	}
	if promo["active"] != true {
		t.Errorf("active=%v want true", promo["active"])
	}
	if promo["text"] != "每日赠送免费额度。" {
		t.Errorf("text=%v", promo["text"])
	}
}

// TestModelListPromotionsAbsentOmitted 上游无 modelPromotions → 条目不带 promotions 字段
// （空值省略，不编造空数组）。
func TestModelListPromotionsAbsentOmitted(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	for _, m := range h.modelList() {
		if _, ok := m["promotions"]; ok {
			t.Errorf("entry %v should omit promotions when upstream has none", m["id"])
		}
	}
}
