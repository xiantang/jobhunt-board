// Package application 是面试流程模块：一条记录代表一次投递（公司 + 岗位），
// 卡片在看板的阶段之间流转，每一轮面试单独记录时间与会议信息。
//
// 阶段规则委托给 workflow 包（阶段配置由 stage 包提供）；
// 成员 / 看板只通过下面的只读接口校验，避免包间强耦合。
package application

import (
	"time"

	"interview/internal/workflow"
)

// Application 是一条面试流程。
type Application struct {
	ID         int64         `json:"id"`
	BoardID    int64         `json:"board_id"`
	BoardKey   string        `json:"board_key"`
	Seq        int           `json:"seq"`
	Key        string        `json:"key"` // 展示用编号，如 JOBHUNT-3
	Company    string        `json:"company"`
	Role       string        `json:"role"`
	Channel    string        `json:"channel"`
	Notes      string        `json:"notes"`
	StageKey   string        `json:"stage_key"`
	StageLabel string        `json:"stage_label"`
	StageKind  workflow.Kind `json:"stage_kind"`
	Intent     string        `json:"intent"`
	OwnerID    *int64        `json:"owner_id"`
	Owner      *Owner        `json:"owner"`
	NextRound  *Round        `json:"next_round"`  // 最近一场待进行的面试，卡片上直接展示
	RoundCount int           `json:"round_count"` // 已经安排过几轮
	Position   float64       `json:"-"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

// Owner 是卡片上展示的跟进人信息（成员表的只读投影）。
type Owner struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Initial 返回头像首字母。
func (o Owner) Initial() string {
	if o.Name == "" {
		return "?"
	}
	return string([]rune(o.Name)[:1])
}

// 意向度取值。
const (
	IntentLow    = "low"
	IntentNormal = "normal"
	IntentHigh   = "high"
)

// IntentLabel 返回意向度中文名。
func (a Application) IntentLabel() string {
	switch a.Intent {
	case IntentHigh:
		return "高"
	case IntentLow:
		return "低"
	default:
		return "中"
	}
}

// NeedsSchedule 表示卡片停在面试 / 任务阶段，却还没定时间——界面上给个提醒。
// 对面试是「还没约上」，对在线测评是「还不知道什么时候截止」，都得催。
//
// 已经面完在等结果的那条不算：事情做完了，没有时间要约，催也没用。
func (a Application) NeedsSchedule() bool {
	if !a.StageKind.TracksRound() {
		return false
	}
	if a.NextRound != nil && a.NextRound.Awaiting() {
		return false
	}
	return a.NextRound == nil || a.NextRound.ScheduledAt == nil
}

// Round 是一轮面试的记录：时间 + 会议方式 + 结果。
type Round struct {
	ID            int64  `json:"id"`
	ApplicationID int64  `json:"application_id"`
	StageKey      string `json:"stage_key"`
	StageLabel    string `json:"stage_label"` // 快照，阶段改名后历史仍可读
	// Kind 决定 ScheduledAt 的含义：面试是开始时间，任务（在线测评）是截止时间。
	Kind         string     `json:"kind"`
	ScheduledAt  *time.Time `json:"scheduled_at"`
	DurationMin  int        `json:"duration_min"`
	Mode         string     `json:"mode"`
	MeetingURL   string     `json:"meeting_url"`
	MeetingPlace string     `json:"meeting_place"`
	Interviewer  string     `json:"interviewer"`
	Result       string     `json:"result"`
	Notes        string     `json:"notes"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ScheduledRound 是一轮面试连同它所属流程的展示信息。
// 日程页要跨看板按时间铺开，光有 Round 拼不出「哪家公司的第几面」，
// 所以这里把 join 出来的字段一并带上。
type ScheduledRound struct {
	RoundID        int64      `json:"round_id"`
	ApplicationID  int64      `json:"application_id"`
	ApplicationKey string     `json:"application_key"`
	BoardKey       string     `json:"board_key"`
	Company        string     `json:"company"`
	Role           string     `json:"role"`
	StageKey       string     `json:"stage_key"`
	StageLabel     string     `json:"stage_label"`
	StageColor     string     `json:"stage_color"`
	Kind           string     `json:"kind"`
	ScheduledAt    *time.Time `json:"scheduled_at"`
	DurationMin    int        `json:"duration_min"`
	Mode           string     `json:"mode"`
	MeetingURL     string     `json:"meeting_url"`
	MeetingPlace   string     `json:"meeting_place"`
	Interviewer    string     `json:"interviewer"`
	Result         string     `json:"result"`
	Notes          string     `json:"notes"`
	GoogleEventID  string     `json:"google_event_id"`
}

// 轮次类型取值。task 是在线测评这类「不约时间、只有 DDL 和一条链接」的任务。
const (
	RoundKindInterview = "interview"
	RoundKindTask      = "task"
)

// IsTask 表示这一轮是任务（在线测评），ScheduledAt 要当截止时间读。
func (r Round) IsTask() bool { return r.Kind == RoundKindTask }

// IsTask 同上，日程页用。
func (r ScheduledRound) IsTask() bool { return r.Kind == RoundKindTask }

// 面试方式取值。
const (
	ModeOnline = "online"
	ModeOnsite = "onsite"
	ModePhone  = "phone"
)

// 面试结果取值。
//
// awaiting 是「面试已经结束、结果还没下来」这段真空期：面完到通知之间常常要等
// 几天甚至一两周，这期间它既不该再催人去约时间，也不该被当成还没进行的面试。
const (
	ResultPending   = "pending"
	ResultAwaiting  = "awaiting"
	ResultPassed    = "passed"
	ResultFailed    = "failed"
	ResultCancelled = "cancelled"
)

var modeLabels = map[string]string{ModeOnline: "线上", ModeOnsite: "现场", ModePhone: "电话"}

var resultLabels = map[string]string{
	ResultPending:   "待进行",
	ResultAwaiting:  "等结果",
	ResultPassed:    "已通过",
	ResultFailed:    "未通过",
	ResultCancelled: "已取消",
}

// ModeLabel 返回面试方式中文名。
func (r Round) ModeLabel() string { return labelOr(modeLabels, r.Mode) }

// ResultLabel 返回面试结果中文名。
func (r Round) ResultLabel() string { return labelOr(resultLabels, r.Result) }

// Awaiting 表示这一轮已经结束，正在等结果。
func (r Round) Awaiting() bool { return r.Result == ResultAwaiting }

// Awaiting 同上，日程页用。
func (r ScheduledRound) Awaiting() bool { return r.Result == ResultAwaiting }

// Scheduled 表示这一轮已经定下时间。
func (r Round) Scheduled() bool { return r.ScheduledAt != nil }

// Overdue 表示时间已过但结果还没回填。
func (r Round) Overdue() bool {
	return r.Result == ResultPending && r.ScheduledAt != nil && r.ScheduledAt.Before(time.Now())
}

// WhenText 返回卡片上展示的时间，未排期时给出提示文案。
func (r Round) WhenText() string {
	if r.ScheduledAt == nil {
		return "待安排"
	}
	return r.ScheduledAt.Local().Format("01-02 15:04")
}

// MeetingText 返回会议方式的一行摘要：线上给链接域名，线下给地点。
func (r Round) MeetingText() string {
	switch {
	case r.MeetingPlace != "":
		return r.MeetingPlace
	case r.MeetingURL != "":
		return "线上会议"
	default:
		return r.ModeLabel()
	}
}

// 操作日志事件类型。
const (
	EventCreated        = "created"
	EventOwnerChanged   = "owner_changed"
	EventStageChanged   = "stage_changed"
	EventUpdated        = "updated"
	EventRoundScheduled = "round_scheduled"
	EventRoundUpdated   = "round_updated"
	EventRoundRemoved   = "round_removed"
)

// Event 是一条操作日志。
// 阶段可以随时改名，所以人话文案在写入时就定稿存进 detail，
// from_stage / to_stage 只留 key 供机器使用。
type Event struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	ActorName string    `json:"actor_name"`
	FromStage string    `json:"from_stage,omitempty"`
	ToStage   string    `json:"to_stage,omitempty"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// Text 渲染成一句人话，页面与接口共用。
func (e Event) Text() string {
	actor := e.ActorName
	if actor == "" {
		actor = "某人"
	}
	if e.Detail == "" {
		return actor + " 更新了流程"
	}
	return actor + " " + e.Detail
}

// Detail 是流程详情：卡片本身 + 面试轮次 + 倒序操作日志。
type Detail struct {
	Application Application `json:"application"`
	Rounds      []Round     `json:"rounds"`
	Events      []Event     `json:"events"`
}

// Filter 是看板筛选条件，零值表示不筛选。
type Filter struct {
	OwnerID  *int64
	StageKey *string
	Upcoming bool // 只看有待进行面试的流程
}

func labelOr(m map[string]string, key string) string {
	if l, ok := m[key]; ok {
		return l
	}
	return key
}
