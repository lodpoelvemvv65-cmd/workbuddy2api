// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"time"

	"workbuddy2api/internal/auth"
)

// PickExcludingForRealm 按 realm 过滤的轮换选号：候选仅限 Realm()==realm 的账号。
// realm=="" 退化为 PickExcluding（现状语义，老调用零改动）。
// 可选做请求级轮换（tried）与模型感知（reqModel，6004 模型豁免照常生效）；
// reqModel 非空时健康口径换成 healthyForModel。realm 不匹配的全冷却兜底同样排除。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// PickExcludingForRealmGroups 在 PickExcludingForRealm 之上再叠加**业务分组**过滤：
// 候选须同时满足 Realm()==realm（realm 非空时）与 MatchesGroups(groups)（groups 非空时）。
//
// 两维正交（realm=技术域、groups=业务标签），AND 组合；任一为空则该维度不过滤。
// realm 不匹配、分组不匹配的账号在**全冷却兜底路径**同样被排除（见
// pickEarliestExpiryLockedScoped）。
//
// 这是生产链路的选号入口：由请求所带密钥的 groups 决定可见账号范围。
func (p *Pool) PickExcludingForRealmGroups(tried map[string]bool, reqModel, realm string, groups []string) *auth.Auth {
	return p.pickScoped(tried, reqModel, realm, groups)
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// DeptestOnly: 仅 realm_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm。保留作 ForModelRealm 的模型维度退化
// （model=""）语义锚点测试。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsLocked(realm, func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.availableUIDsLocked(realm,
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}
