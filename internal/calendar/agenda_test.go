package calendar

import (
	"testing"

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
