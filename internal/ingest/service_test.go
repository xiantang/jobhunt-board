package ingest

import (
	"testing"
	"time"

	"interview/internal/application"
	"interview/internal/stage"
	"interview/internal/workflow"
)

func TestNormalizeCompany(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foodpanda", "foodpanda"},
		{"Foodpanda 台湾", "foodpanda台湾"},
		{"字节跳动有限公司", "字节跳动"},
		{"Acme Inc.", "acme"},
		{"  ", ""},
	}
	for _, c := range cases {
		if got := normalizeCompany(c.in); got != c.want {
			t.Errorf("normalizeCompany(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestMatchCompany(t *testing.T) {
	existing := []application.Application{
		{ID: 1, Company: "Foodpanda 台湾"},
		{ID: 2, Company: "字节跳动"},
	}

	if got := matchCompany("foodpanda", existing); got == nil || got.ID != 1 {
		t.Fatalf("「foodpanda」应当命中 Foodpanda 台湾：%v", got)
	}
	if got := matchCompany("字节跳动有限公司", existing); got == nil || got.ID != 2 {
		t.Fatalf("去掉后缀后应当精确命中：%v", got)
	}
	if got := matchCompany("美团", existing); got != nil {
		t.Fatalf("不相干的公司不该命中：%v", got)
	}
	// 两个字的名字用包含匹配太容易误伤，只走精确匹配。
	if got := matchCompany("台湾", existing); got != nil {
		t.Fatalf("过短的名字不该走包含匹配：%v", got)
	}
}

func TestParseTimeRejectsGarbage(t *testing.T) {
	rfc := "2026-08-25T14:00:00+08:00"
	if got := parseTime(&rfc); got == nil || got.UTC().Hour() != 6 {
		t.Fatalf("RFC3339 应当解析成功：%v", got)
	}
	loose := "2026-08-25 14:00"
	if got := parseTime(&loose); got == nil {
		t.Fatalf("缺时区的写法也应当兜住：%v", got)
	}
	// 解析不出来时宁可当作「待安排」，也不要塞一个错的时间进去。
	for _, bad := range []string{"", "下周三", "尽快"} {
		if got := parseTime(&bad); got != nil {
			t.Fatalf("parseTime(%q) = %v，期望 nil", bad, got)
		}
	}
	if got := parseTime(nil); got != nil {
		t.Fatalf("parseTime(nil) = %v，期望 nil", got)
	}
}

// testStages 是一套精简的阶段配置：在线测评可跳过，一面不可跳过。
func testStages() []stage.Stage {
	return []stage.Stage{
		{Key: "hr_screen", Label: "HR screen", Kind: workflow.KindNormal},
		{Key: "online_test", Label: "在线测评", Kind: workflow.KindNormal, Skippable: true},
		{Key: "round_1", Label: "一面", Kind: workflow.KindInterview},
		{Key: "round_2", Label: "二面", Kind: workflow.KindInterview},
	}
}

func TestToDraftFoldsDeadlineIntoNotes(t *testing.T) {
	var ex extraction
	ex.Company = "foodpanda"
	ex.StageKey = "online_test"
	deadline := "2026-08-28"
	ex.Round.Deadline = &deadline
	ex.Round.Notes = "在线技术测评"

	stages := testStages()
	d := (&Service{now: time.Now}).toDraft(ex, stages, nil, stage.Flow(stages))

	if d.StageLabel != "在线测评" {
		t.Fatalf("阶段展示名 = %q", d.StageLabel)
	}
	if d.Round.ScheduledAt != nil {
		t.Fatalf("截止日期不该被当成排期：%v", d.Round.ScheduledAt)
	}
	if d.Round.Notes != "截止：2026-08-28。在线技术测评" {
		t.Fatalf("备注 = %q", d.Round.Notes)
	}
}

func TestToDraftWarnsOnIllegalTransition(t *testing.T) {
	var ex extraction
	ex.Company = "字节跳动"
	ex.StageKey = "round_2" // 中间隔着不可跳过的「一面」

	stages := testStages()
	existing := []application.Application{{ID: 7, Company: "字节跳动", StageKey: "hr_screen"}}
	d := (&Service{now: time.Now}).toDraft(ex, stages, existing, stage.Flow(stages))

	if d.Match == nil || d.Match.ID != 7 {
		t.Fatalf("应当命中已有卡片：%v", d.Match)
	}
	if d.StageAllowed || d.StageWarning == "" {
		t.Fatalf("HR screen → 二面 不合法，应当给出提示：%+v", d)
	}
}

func TestToDraftDropsUnknownStageKey(t *testing.T) {
	var ex extraction
	ex.Company = "某公司"
	ex.StageKey = "hallucinated_stage"

	stages := testStages()
	d := (&Service{now: time.Now}).toDraft(ex, stages, nil, stage.Flow(stages))

	if d.StageKey != "" || d.StageLabel != "" {
		t.Fatalf("模型编出来的阶段应当被丢掉：%q / %q", d.StageKey, d.StageLabel)
	}
}
