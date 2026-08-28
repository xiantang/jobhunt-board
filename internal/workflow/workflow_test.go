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

// skippableFlow 把「在线测评」标成可跳过，另加一个不可跳过的二面用来验证边界。
func skippableFlow() Flow {
	return NewFlow([]Stage{
		{Key: "hr_screen", Label: "HR screen", Kind: KindNormal},
		{Key: "online_test", Label: "在线测评", Kind: KindNormal, Skippable: true},
		{Key: "round_1", Label: "一面", Kind: KindInterview},
		{Key: "round_2", Label: "二面", Kind: KindInterview},
		{Key: "round_3", Label: "三面", Kind: KindInterview},
		{Key: "offer", Label: "已发 Offer", Kind: KindOffer},
	})
}

func TestSkippableStage(t *testing.T) {
	flow := skippableFlow()

	cases := []struct {
		name     string
		from, to string
		want     bool
	}{
		{"跨过可跳过的列", "hr_screen", "round_1", true},
		{"反向也能跨回来", "round_1", "hr_screen", true},
		{"相邻依然放行", "round_1", "round_2", true},
		{"中间隔着不可跳过的列仍被拒", "hr_screen", "round_2", false},
		{"隔两列不可跳过更要被拒", "online_test", "round_3", false},
		{"终态不受影响", "hr_screen", "offer", true},
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
			if got := flow.Can(from, to); got != tc.want {
				t.Fatalf("Can(%s → %s) = %v，期望 %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestNextIncludesSkippedStage(t *testing.T) {
	flow := skippableFlow()
	from, _ := flow.Parse("hr_screen")

	got := make([]string, 0, 4)
	for _, s := range flow.Next(from) {
		got = append(got, s.Key)
	}
	// 跨过可跳过的「在线测评」后，一面也进入可选列表；二面仍然进不去。
	want := []string{"online_test", "round_1", "offer"}
	if len(got) != len(want) {
		t.Fatalf("可流转阶段 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("可流转阶段 = %v，期望 %v", got, want)
		}
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

// waitingFlow 是真实看板的形状：三面后面挂着一列「等待回复中」。
// 它摆在那儿只是位置——一面、二面、三面面完都可能卡在等通知。
func waitingFlow() Flow {
	return NewFlow([]Stage{
		{Key: "round_1", Label: "一面", Kind: KindInterview},
		{Key: "round_2", Label: "二面", Kind: KindInterview},
		{Key: "round_3", Label: "三面", Kind: KindInterview},
		{Key: "waiting_reply", Label: "等待回复中", Kind: KindWaiting, Skippable: true},
		{Key: "round_4", Label: "四面", Kind: KindInterview},
		{Key: "rejected", Label: "已结束", Kind: KindRejected},
	})
}

func TestWaitingStageIgnoresColumnOrder(t *testing.T) {
	flow := waitingFlow()

	cases := []struct {
		name     string
		from, to string
		want     bool
	}{
		// 这一条是这一列存在的理由：一面和它之间隔着二面、三面两列不可跳过的，
		// 按顺序规则会被拒，而一面面完在等通知是最常见的情形。
		{"一面直接进等待回复", "round_1", "waiting_reply", true},
		{"二面直接进等待回复", "round_2", "waiting_reply", true},
		{"三面进等待回复", "round_3", "waiting_reply", true},
		{"等回复之后去二面", "waiting_reply", "round_2", true},
		{"等回复之后去四面", "waiting_reply", "round_4", true},
		{"等回复之后被挂", "waiting_reply", "rejected", true},
		// 它可跳过，所以照样挡不住三面直接推进到四面。
		{"它不挡住正常推进", "round_3", "round_4", true},
		// 其余规则不受影响：一面还是不能直接跳到三面。
		{"普通跨列仍然被拒", "round_1", "round_3", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, _ := flow.Parse(tc.from)
			to, _ := flow.Parse(tc.to)
			if got := flow.Can(from, to); got != tc.want {
				t.Fatalf("Can(%s → %s) = %v，期望 %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestWaitingKindIsNotTerminal(t *testing.T) {
	if KindWaiting.Terminal() {
		t.Fatal("等待回复不是终态：流程还在继续，只是在等通知")
	}
	if !KindWaiting.Unordered() {
		t.Fatal("等待回复应当不参与列顺序")
	}
	if KindWaiting.TracksRound() {
		t.Fatal("等待回复列不该自动挂一条待办——要做的事在上一列已经做完了")
	}
}
