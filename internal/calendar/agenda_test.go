package calendar

import (
	"testing"
	"time"

	"interview/internal/application"
	"interview/internal/gcal"
)

// 同步出去的面试会被 ListEvents 原样拉回来。不滤掉的话，同一场面试
// 在日程上出现两次，还会互相标成「撞期」——那是我们自己造出来的假冲突。
func TestDropOwnEvents(t *testing.T) {
	events := []gcal.Event{
		{ID: "ev-mine", Summary: "面试：Bitdeer · 一面"},
		{ID: "ev-theirs", Summary: "Video Interview Invitation from Xiaomi"},
	}
	rounds := []application.ScheduledRound{
		{RoundID: 1, GoogleEventID: "ev-mine"},
		{RoundID: 2, GoogleEventID: ""}, // 还没同步的那轮不该影响过滤
	}

	got := dropOwnEvents(events, rounds)
	if len(got) != 1 || got[0].ID != "ev-theirs" {
		t.Fatalf("只该留下招聘方发来的那条，实际 %+v", got)
	}

	// 一条都没同步过时原样返回，别白跑一趟拷贝。
	if got := dropOwnEvents(events, []application.ScheduledRound{{RoundID: 3}}); len(got) != 2 {
		t.Fatalf("没有已同步的轮次时不该滤掉任何事件，实际 %+v", got)
	}
}

// 任务的 ScheduledAt 是截止时刻，不是开始时刻。日程上这一格必须
// 结束在 DDL 上、往前占出高度，否则画出来会变成「DDL 之后才开始」。
func TestInterviewEntryTaskEndsAtDeadline(t *testing.T) {
	due := time.Date(2026, 8, 26, 18, 0, 0, 0, time.Local)
	e := interviewEntry(application.ScheduledRound{
		RoundID:     7,
		Company:     "小米",
		StageLabel:  "在线测评",
		Kind:        application.RoundKindTask,
		ScheduledAt: &due,
		DurationMin: 0,
	})

	if e.Source != SourceTask {
		t.Fatalf("source = %q，期望 task", e.Source)
	}
	if !e.End.Equal(due) {
		t.Fatalf("这一格该结束在 DDL %v，实际 %v", due, e.End)
	}
	if got := due.Sub(e.Start); got != TaskBlockMin*time.Minute {
		t.Fatalf("这一格该往前占 %d 分钟，实际 %v", TaskBlockMin, got)
	}
	if e.Due == nil || !e.Due.Equal(due) {
		t.Fatalf("due 该原样带出 %v，实际 %v", due, e.Due)
	}
}

// DDL 是「这之前做完」，不是「这个时段被占住」——它和同时刻的面试不算撞期。
func TestMarkConflictsIgnoresTasks(t *testing.T) {
	start := time.Date(2026, 8, 26, 17, 45, 0, 0, time.Local)
	entries := []Entry{
		{Source: SourceInterview, Start: start, End: start.Add(time.Hour)},
		{Source: SourceTask, Start: start, End: start.Add(15 * time.Minute)},
	}
	markConflicts(entries)
	for i, e := range entries {
		if e.Conflict {
			t.Fatalf("第 %d 条被标成了撞期，但面试和 DDL 不冲突：%+v", i, e)
		}
	}
}

// 日程页按自然周显示：不管今天周几，起点都是那一周的周日零点。
func TestStartOfWeek(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"周六", time.Date(2026, 8, 22, 16, 40, 0, 0, time.Local), time.Date(2026, 8, 16, 0, 0, 0, 0, time.Local)},
		{"周日当天", time.Date(2026, 8, 23, 0, 30, 0, 0, time.Local), time.Date(2026, 8, 23, 0, 0, 0, 0, time.Local)},
		{"周三", time.Date(2026, 8, 26, 23, 59, 0, 0, time.Local), time.Date(2026, 8, 23, 0, 0, 0, 0, time.Local)},
		// 跨月：8 月 1 日是周六，那一周从 7 月 26 日开始。
		{"跨月", time.Date(2026, 8, 1, 9, 0, 0, 0, time.Local), time.Date(2026, 7, 26, 0, 0, 0, 0, time.Local)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StartOfWeek(c.at); !got.Equal(c.want) {
				t.Fatalf("StartOfWeek(%v) = %v，期望 %v", c.at, got, c.want)
			}
		})
	}
}
