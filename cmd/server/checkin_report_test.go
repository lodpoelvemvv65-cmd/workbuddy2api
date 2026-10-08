package main

import (
	"testing"

	"workbuddy2api/internal/scheduler"
)

// TestBuildCheckinReportMapping 计数与 realm 归属必须准 —— 这两处错了不会报错，
// 只会让控制台面板显示错数（比如把 skipped 算成 ok、global 号标成 cn），必须锁住。
func TestBuildCheckinReportMapping(t *testing.T) {
	c1 := int64(2100)
	c2 := int64(479)
	outcomes := []scheduler.CheckinOutcome{
		{UID: "cn-1", Nickname: "online", Status: scheduler.CheckinOK, Credits: &c1},
		{UID: "cn-2", Nickname: "clay", Status: scheduler.CheckinAlready},
		{UID: "gl-1", Nickname: "655buttr", Status: scheduler.CheckinSkipped, Detail: "global", Credits: &c2},
		{UID: "gl-2", Nickname: "broken", Status: scheduler.CheckinFail, Detail: "refresh failed"},
		{UID: "ghost", Nickname: "gone", Status: scheduler.CheckinSkipped, Detail: "no credentials"},
	}
	realmOf := func(uid string) string {
		switch uid {
		case "cn-1", "cn-2":
			return "cn"
		case "gl-1", "gl-2":
			return "global"
		}
		return "" // 池里查不到（如刚被移除）
	}

	rep := buildCheckinReport(outcomes, realmOf, true, []int{9, 21})

	if !rep.Enabled || len(rep.Hours) != 2 {
		t.Errorf("排程字段未透出: %+v", rep)
	}
	if rep.Total != 5 {
		t.Errorf("total=%d want 5", rep.Total)
	}
	if rep.OK != 1 || rep.Already != 1 || rep.Fail != 1 || rep.Skipped != 2 {
		t.Errorf("计数不符: ok=%d already=%d fail=%d skipped=%d（want 1/1/1/2）",
			rep.OK, rep.Already, rep.Fail, rep.Skipped)
	}
	if len(rep.Results) != 5 {
		t.Fatalf("results 数量=%d want 5", len(rep.Results))
	}
	// 顺序必须与输入一致（面板按行对齐 uid/昵称，乱了就对错人）。
	for i, wantUID := range []string{"cn-1", "cn-2", "gl-1", "gl-2", "ghost"} {
		if rep.Results[i].UID != wantUID {
			t.Errorf("results[%d].uid=%q want %q", i, rep.Results[i].UID, wantUID)
		}
	}
	if rep.Results[0].Realm != "cn" || rep.Results[2].Realm != "global" {
		t.Errorf("realm 归属错: %+v", rep.Results)
	}
	// 池里查不到的账号 realm 留空（不猜、不默认成 cn）。
	if rep.Results[4].Realm != "" {
		t.Errorf("查不到的账号 realm 应为空，实际 %q", rep.Results[4].Realm)
	}
	// 余额与 detail 原样透出。
	if rep.Results[0].Credits == nil || *rep.Results[0].Credits != 2100 {
		t.Errorf("cn 余额丢失: %+v", rep.Results[0])
	}
	if rep.Results[2].Credits == nil || *rep.Results[2].Credits != 479 {
		t.Errorf("global 余额丢失: %+v", rep.Results[2])
	}
	if rep.Results[3].Detail != "refresh failed" {
		t.Errorf("失败原因丢失: %+v", rep.Results[3])
	}
}

// TestBuildCheckinReportEmpty 空池必须给出 []（不是 nil），否则前端 .map 会炸。
func TestBuildCheckinReportEmpty(t *testing.T) {
	rep := buildCheckinReport(nil, func(string) string { return "" }, false, nil)
	if rep.Total != 0 || rep.Results == nil || len(rep.Results) != 0 {
		t.Fatalf("空池应给出 total=0 且 results 非 nil 空切片: %+v", rep)
	}
}
