package workflow

import (
	"errors"
	"testing"

	"interview/internal/platform/apperr"
)

// demoFlow 是测试用的一套阶段：两个普通阶段 + 一个要求跟进人的面试阶段 + 两个终态。
func demoFlow() Flow {
	return NewFlow([]Stage{
		{Key: "hr_screen", Label: "HR screen", Kind: KindNormal},
		{Key: "online_test", Label: "在线测评", Kind: KindNormal},
		{Key: "round_1", Label: "一面", Kind: KindInterview, RequiresOwner: true},
		{Key: "offer", Label: "已发 Offer", Kind: KindOffer},
		{Key: "rejected", Label: "已结束", Kind: KindRejected},
	})
}

func TestValidate(t *testing.T) {
	flow := demoFlow()

	cases := []struct {
		name     string
		from, to string
		hasOwner bool
		wantCode string // 空表示应当通过
	}{
		{"相邻推进", "hr_screen", "online_test", false, ""},
		{"相邻回退", "online_test", "hr_screen", false, ""},
		{"同阶段排序", "online_test", "online_test", false, ""},
		{"跨阶段跳跃被拒", "hr_screen", "round_1", true, apperr.CodeInvalidTransition},
		{"任意阶段直达 Offer", "hr_screen", "offer", false, ""},
		{"任意阶段直达结束", "online_test", "rejected", false, ""},
		{"终态退回非终态", "rejected", "hr_screen", false, ""},
		{"终态之间互转", "offer", "rejected", false, ""},
		{"进入面试阶段需跟进人", "online_test", "round_1", false, apperr.CodeOwnerRequired},
		{"有跟进人则放行", "online_test", "round_1", true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, err := flow.Parse(tc.from)
			if err != nil {
				t.Fatalf("解析起始阶段失败: %v", err)
			}
			to, err := flow.Parse(tc.to)
			if err != nil {
				t.Fatalf("解析目标阶段失败: %v", err)
			}

			err = flow.Validate(from, to, tc.hasOwner)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("期望通过，实际被拒: %v", err)
				}
				return
			}
			var appErr *apperr.Error
			if !errors.As(err, &appErr) {
				t.Fatalf("期望 apperr，实际 %v", err)
			}
			if appErr.Code != tc.wantCode {
				t.Fatalf("错误码 = %s，期望 %s", appErr.Code, tc.wantCode)
			}
		})
	}
}

func TestParseUnknownStage(t *testing.T) {
	if _, err := demoFlow().Parse("nope"); err == nil {
		t.Fatal("未知阶段应当报错")
	}
}

func TestNext(t *testing.T) {
	flow := demoFlow()
	from, _ := flow.Parse("hr_screen")

	got := make([]string, 0, 4)
	for _, s := range flow.Next(from) {
		got = append(got, s.Key)
	}
	// 相邻的 online_test + 两个终态，不含跨阶段的 round_1。
	want := []string{"online_test", "offer", "rejected"}
	if len(got) != len(want) {
		t.Fatalf("可流转阶段 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("可流转阶段 = %v，期望 %v", got, want)
		}
	}
}

func TestEntryOnEmptyFlow(t *testing.T) {
	if _, err := NewFlow(nil).Entry(); err == nil {
		t.Fatal("没有阶段时 Entry 应当报错")
	}
}
