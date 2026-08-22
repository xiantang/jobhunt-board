package ingest

import (
	"strings"
	"testing"
	"time"

	"interview/internal/application"
	"interview/internal/stage"
	"interview/internal/workflow"
)

func TestParseCompany(t *testing.T) {
	cases := []struct {
		in     string
		canon  string
		tokens []string
	}{
		{"foodpanda", "foodpanda", []string{"foodpanda"}},
		{"Foodpanda 台湾", "foodpanda台湾", []string{"foodpanda", "台湾"}},
		{"字节跳动有限公司", "字节跳动", []string{"字节跳动"}},
		{"Acme Inc.", "acme", []string{"acme"}},
		{"Anker Innovations", "ankerinnovations", []string{"anker", "innovations"}},
		{"  ", "", nil},
	}
	for _, c := range cases {
		got := parseCompany(c.in)
		if got.canonical != c.canon {
			t.Errorf("parseCompany(%q).canonical = %q，期望 %q", c.in, got.canonical, c.canon)
		}
		if strings.Join(got.tokens, "|") != strings.Join(c.tokens, "|") {
			t.Errorf("parseCompany(%q).tokens = %v，期望 %v", c.in, got.tokens, c.tokens)
		}
	}
}

func TestMatchCompany(t *testing.T) {
	existing := []application.Application{
		{ID: 1, Company: "Foodpanda 台湾"},
		{ID: 2, Company: "字节跳动"},
		{ID: 3, Company: "Bitdeer"},
		{ID: 4, Company: "易点天下网络科技股份有限公司"},
		{ID: 5, Company: "Anker Innovations"},
	}

	hit := []struct {
		name string
		want int64
	}{
		{"foodpanda", 1}, // 整词命中「Foodpanda 台湾」里的一个词
		{"FoodPanda", 1}, // 大小写无关
		{"字节跳动有限公司", 2},  // 后缀不参与比较
		{"Bitdeer", 3},   // 精确
		{"易点天下", 4},      // 中文前缀：字号 + 业务描述说的是同一家
		{"Anker", 5},     // 整词命中多词名字里的一个
	}
	for _, c := range hit {
		got := matchCompany(c.name, existing)
		if got == nil || got.ID != c.want {
			t.Errorf("matchCompany(%q) = %v，期望命中 id=%d", c.name, got, c.want)
		}
	}

	miss := []string{
		"BIT",          // 不是 Bitdeer 的简写，只是恰好是它的前缀
		"Bit",          //
		"Deer",         // 后缀同理，英文不做部分词匹配
		"美团",           // 完全不相干
		"中",            // 单字不参与前缀匹配
		"Innovations",  // 通用后缀词，不是字号——字号在最前面
		"台湾",           // 同理，Foodpanda 台湾 的身份是 foodpanda
		"foodpanda 香港", // 首词对上了，但其余的词对不上，不是同一家
	}
	for _, name := range miss {
		if got := matchCompany(name, existing); got != nil {
			t.Errorf("matchCompany(%q) 不该命中，实际命中了「%s」", name, got.Company)
		}
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
