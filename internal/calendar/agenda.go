package calendar

import (
	"context"
	"sort"
	"strings"
	"time"

	"interview/internal/application"
	"interview/internal/gcal"
)

// 时间线上的两种来源。
const (
	SourceInterview = "interview" // 看板上的面试
	SourceGoogle    = "google"    // Google 日历上已有的会议
)

// Entry 是时间线上的一格，两种来源共用一个结构，前端只按 source 换配色。
type Entry struct {
	Source string    `json:"source"`
	Title  string    `json:"title"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	AllDay bool      `json:"all_day"`

	// 面试专有
	RoundID        int64  `json:"round_id,omitempty"`
	ApplicationID  int64  `json:"application_id,omitempty"`
	ApplicationKey string `json:"application_key,omitempty"`
	Company        string `json:"company,omitempty"`
	StageLabel     string `json:"stage_label,omitempty"`
	StageColor     string `json:"stage_color,omitempty"`
	Mode           string `json:"mode,omitempty"`
	Result         string `json:"result,omitempty"`
	Synced         bool   `json:"synced"` // 已经推到 Google 日历上了

	// 共有的会议信息
	Location string `json:"location,omitempty"`
	URL      string `json:"url,omitempty"`

	// Conflict 表示这一格和同一天的另一格时间重叠。
	Conflict bool `json:"conflict"`
}

// Day 是日程页上的一天。
type Day struct {
	Date    string  `json:"date"`    // 2026-08-25
	Weekday string  `json:"weekday"` // 周一
	Today   bool    `json:"today"`
	Entries []Entry `json:"entries"`
}

// Agenda 是日程页的全部数据。
type Agenda struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Days        []Day     `json:"days"`
	Unscheduled []Entry   `json:"unscheduled"` // 停在面试阶段但还没定时间的
	Status      Status    `json:"status"`
	// Warning 是「日历没拉回来」的原因。看板那半边照常显示，不因为
	// Google 挂了就整页报错。
	Warning string `json:"warning"`
}

var weekdays = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// Load 组装日程页：看板的面试 + Google 日历的会议，按天铺开。
//
// 两边任何一边失败都不该让整页空掉——Google 拉不回来时只挂个 warning，
// 看板的面试照常显示。
func (s *Service) Load(ctx context.Context, from time.Time, days int) (Agenda, error) {
	if days <= 0 {
		days = DefaultWindowDays
	}
	from = startOfDay(from)
	to := from.AddDate(0, 0, days)

	status, err := s.Status(ctx)
	if err != nil {
		return Agenda{}, err
	}
	agenda := Agenda{From: from, To: to, Status: status}

	rounds, err := s.rounds.UpcomingRounds(ctx, from, to)
	if err != nil {
		return Agenda{}, err
	}
	entries := make([]Entry, 0, len(rounds)+16)
	for _, r := range rounds {
		entries = append(entries, interviewEntry(r))
	}

	if status.Connected {
		events, err := s.listEvents(ctx, from, to)
		if err != nil {
			agenda.Warning = "Google 日历没能拉回来：" + apperrMessage(err)
		} else {
			for _, e := range events {
				entries = append(entries, googleEntry(e))
			}
		}
	}

	unscheduled, err := s.rounds.UnscheduledRounds(ctx)
	if err != nil {
		return Agenda{}, err
	}
	for _, r := range unscheduled {
		agenda.Unscheduled = append(agenda.Unscheduled, interviewEntry(r))
	}

	agenda.Days = bucketByDay(entries, from, days)
	return agenda, nil
}

func (s *Service) listEvents(ctx context.Context, from, to time.Time) ([]gcal.Event, error) {
	token, calendarID, err := s.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	return s.client.ListEvents(ctx, token, calendarID, from, to)
}

func interviewEntry(r application.ScheduledRound) Entry {
	e := Entry{
		Source:         SourceInterview,
		RoundID:        r.RoundID,
		Title:          r.Company + " · " + r.StageLabel,
		Company:        r.Company,
		StageLabel:     r.StageLabel,
		StageColor:     r.StageColor,
		ApplicationID:  r.ApplicationID,
		ApplicationKey: r.ApplicationKey,
		Mode:           r.Mode,
		Result:         r.Result,
		Location:       r.MeetingPlace,
		URL:            r.MeetingURL,
		Synced:         r.GoogleEventID != "",
	}
	if r.ScheduledAt != nil {
		e.Start = *r.ScheduledAt
		e.End = r.ScheduledAt.Add(time.Duration(r.DurationMin) * time.Minute)
	}
	return e
}

func googleEntry(ev gcal.Event) Entry {
	title := ev.Summary
	if title == "" {
		title = "（无标题）"
	}
	return Entry{
		Source:   SourceGoogle,
		Title:    title,
		Start:    ev.Start,
		End:      ev.End,
		AllDay:   ev.AllDay,
		Location: ev.Location,
		URL:      ev.HTMLLink,
	}
}

// bucketByDay 把条目按天分桶，天数固定——空的那天也要占一格，
// 否则一周视图会因为没安排而塌成两三行，看不出「哪天是空的」。
func bucketByDay(entries []Entry, from time.Time, days int) []Day {
	today := startOfDay(time.Now())
	out := make([]Day, 0, days)

	for i := 0; i < days; i++ {
		d := from.AddDate(0, 0, i)
		day := Day{
			Date:    d.Format("2006-01-02"),
			Weekday: weekdays[int(d.Weekday())],
			Today:   d.Equal(today),
			Entries: []Entry{},
		}
		for _, e := range entries {
			if startOfDay(e.Start).Equal(d) {
				day.Entries = append(day.Entries, e)
			}
		}
		sort.SliceStable(day.Entries, func(a, b int) bool {
			if day.Entries[a].AllDay != day.Entries[b].AllDay {
				return day.Entries[a].AllDay // 全天事件排在最前
			}
			return day.Entries[a].Start.Before(day.Entries[b].Start)
		})
		markConflicts(day.Entries)
		out = append(out, day)
	}
	return out
}

// markConflicts 标出同一天里时间重叠的条目。
// 这正是「两边一起看」的意义：面试和已有会议撞上了，得看得见。
func markConflicts(entries []Entry) {
	for i := range entries {
		if entries[i].AllDay {
			continue
		}
		for j := range entries {
			if i == j || entries[j].AllDay {
				continue
			}
			if entries[i].Start.Before(entries[j].End) && entries[j].Start.Before(entries[i].End) {
				entries[i].Conflict = true
				break
			}
		}
	}
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Local().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// apperrMessage 取错误里给用户看的那句话。
func apperrMessage(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i > 0 && i < 40 {
		msg = msg[i+2:]
	}
	return msg
}
