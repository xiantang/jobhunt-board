package calendar

import (
	"context"
	"fmt"
	"strings"
	"time"

	"interview/internal/application"
	"interview/internal/gcal"
	"interview/internal/platform/apperr"
)

// SyncRound 把一轮面试推到 Google 日历：没同步过就新建，同步过就改。
//
// 三种情况会把日程从日历上撤掉：时间被清空、这一轮被标成已取消、
// 或者轮次本身被删除（走 RemoveRound）。
func (s *Service) SyncRound(ctx context.Context, roundID int64) error {
	r, err := s.rounds.RoundForSync(ctx, roundID)
	if err != nil {
		return err
	}

	if r.ScheduledAt == nil || r.Result == application.ResultCancelled {
		return s.removeEvent(ctx, r.RoundID, r.GoogleEventID)
	}

	token, calendarID, err := s.accessToken(ctx)
	if err != nil {
		return err
	}
	input := eventInput(r)

	if r.GoogleEventID != "" {
		err := s.client.UpdateEvent(ctx, token, calendarID, r.GoogleEventID, input)
		if err == nil {
			return nil
		}
		// 用户可能在 Google 那边把日程删了，这时改会 404——退回去重建一条。
		var apiErr *gcal.APIError
		if !gcal.AsAPIError(err, &apiErr) || (apiErr.Status != 404 && apiErr.Status != 410) {
			return apperr.Conflict(apperr.CodeGoogleNotConnected, "同步到 Google 日历失败："+err.Error())
		}
	}

	eventID, err := s.client.CreateEvent(ctx, token, calendarID, input)
	if err != nil {
		return apperr.Conflict(apperr.CodeGoogleNotConnected, "同步到 Google 日历失败："+err.Error())
	}
	return s.rounds.SetGoogleEventID(ctx, r.RoundID, eventID)
}

// RemoveRound 在轮次被删掉之前，把它在日历上的日程一并撤掉。
// 必须在删库之前调用——删完就查不到 google_event_id 了。
func (s *Service) RemoveRound(ctx context.Context, roundID int64) error {
	r, err := s.rounds.RoundForSync(ctx, roundID)
	if err != nil {
		return err
	}
	return s.removeEvent(ctx, r.RoundID, r.GoogleEventID)
}

func (s *Service) removeEvent(ctx context.Context, roundID int64, eventID string) error {
	if eventID == "" {
		return nil
	}
	token, calendarID, err := s.accessToken(ctx)
	if err != nil {
		return err
	}
	if err := s.client.DeleteEvent(ctx, token, calendarID, eventID); err != nil {
		return apperr.Conflict(apperr.CodeGoogleNotConnected, "从 Google 日历删除日程失败："+err.Error())
	}
	return s.rounds.SetGoogleEventID(ctx, roundID, "")
}

// eventInput 把一轮面试 / 一条任务翻译成一条日程。
// 标题带上「面试」「截止」这样的前缀，是为了在一堆会议里一眼认出来。
//
// 任务的 ScheduledAt 是 DDL：日程结束在那个时刻、往前占 TaskBlockMin 分钟，
// 这样在 Google 日历上看到的就是「截止线压在 18:00」，而不是「18:00 开始干活」。
func eventInput(r application.ScheduledRound) gcal.EventInput {
	start := *r.ScheduledAt
	duration := time.Duration(r.DurationMin) * time.Minute
	if duration <= 0 {
		duration = time.Hour
	}
	if r.IsTask() {
		duration = TaskBlockMin * time.Minute
		start = r.ScheduledAt.Add(-duration)
	}

	what, subject := "面试", r.Company
	if r.IsTask() {
		what = "截止"
	}
	if r.Role != "" {
		subject = r.Company + " " + r.Role
	}
	title := fmt.Sprintf("%s：%s · %s", what, subject, r.StageLabel)

	var desc strings.Builder
	fmt.Fprintf(&desc, "看板流程：%s\n", r.ApplicationKey)
	if r.IsTask() && r.ScheduledAt != nil {
		fmt.Fprintf(&desc, "截止时间：%s\n", r.ScheduledAt.Local().Format("2006-01-02 15:04"))
	}
	if r.Interviewer != "" {
		fmt.Fprintf(&desc, "面试官：%s\n", r.Interviewer)
	}
	if r.MeetingURL != "" {
		label := "会议链接"
		if r.IsTask() {
			label = "测评链接"
		}
		fmt.Fprintf(&desc, "%s：%s\n", label, r.MeetingURL)
	}
	if r.Notes != "" {
		fmt.Fprintf(&desc, "\n%s\n", r.Notes)
	}
	desc.WriteString("\n（由面试看板自动同步，改动请回看板操作）")

	location := r.MeetingPlace
	if location == "" && r.MeetingURL != "" {
		location = r.MeetingURL
	}

	return gcal.EventInput{
		Summary:     title,
		Description: desc.String(),
		Location:    location,
		Start:       start,
		End:         start.Add(duration),
	}
}
