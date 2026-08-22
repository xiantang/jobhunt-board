// Package ingest 是「AI 录入」模块：把一段自由文本（多半是 HR 邮件）
// 解析成一份可编辑的草稿，用户确认后才写进看板。
//
// 拆成两步是刻意的——模型会抠错公司名和阶段，
// 草稿阶段出错只是界面上改一改，落库之后出错就得手动回滚。
package ingest

import (
	"time"

	"interview/internal/application"
	"interview/internal/stage"
)

// Draft 是解析结果，前端拿它预填确认表单。
// 所有字段都可以在界面上改，后端不把它当可信输入。
type Draft struct {
	Company string `json:"company"`
	Role    string `json:"role"`
	Channel string `json:"channel"` // 邮件里能看出来的投递渠道，如「官网」「内推」
	Notes   string `json:"notes"`   // 给卡片的备注，一般是原文摘要

	StageKey   string `json:"stage_key"`   // 建议落到哪一列，取自本看板的阶段配置
	StageLabel string `json:"stage_label"` // 该列的展示名，为空表示模型没认出来

	Round DraftRound `json:"round"` // 这一轮面试 / 测评的信息

	Summary    string `json:"summary"`    // 一句话说明这封邮件是什么
	Confidence string `json:"confidence"` // high / medium / low

	// Match 是按公司名匹配到的已有卡片，非空时确认操作会复用它而不是新建。
	Match *application.Application `json:"match"`
	// StageAllowed 表示从卡片当前阶段流转到 StageKey 是否合法。
	// 新建卡片时恒为 true——新卡落在第一列，随后按需流转。
	StageAllowed bool   `json:"stage_allowed"`
	StageWarning string `json:"stage_warning"`
}

// DraftRound 是要录入的这一轮的信息。
type DraftRound struct {
	Create       bool       `json:"create"` // 是否要建这一轮记录
	ScheduledAt  *time.Time `json:"scheduled_at"`
	DurationMin  int        `json:"duration_min"`
	Mode         string     `json:"mode"` // online / onsite / phone
	MeetingURL   string     `json:"meeting_url"`
	MeetingPlace string     `json:"meeting_place"`
	Interviewer  string     `json:"interviewer"`
	Notes        string     `json:"notes"`
	// Deadline 是邮件里的截止日期（如测评链接过期时间）。
	// 它不是面试时间，所以单独一个字段：落在任务阶段（在线测评）时会成为
	// 这一轮的 DDL，别的阶段就只折进 Notes 里展示。
	Deadline string `json:"deadline"`
}

// ConfirmInput 是用户在草稿上改完之后提交的落库请求。
// 到这一步模型已经不参与了，走的都是普通的创建 / 流转 / 排期逻辑。
type ConfirmInput struct {
	ApplicationID int64 // > 0 表示复用已有卡片，否则新建
	Company       string
	Role          string
	Channel       string
	Notes         string
	OwnerID       *int64
	Intent        string
	StageKey      string // 为空表示不流转
	Round         DraftRound
}

// Result 是落库结果，给前端做提示文案。
type Result struct {
	Application application.Application `json:"application"`
	Round       *application.Round      `json:"round"`
	Created     bool                    `json:"created"` // 新建了卡片（false 表示复用已有卡片）
	Moved       bool                    `json:"moved"`   // 发生了阶段流转
}

// stageBrief 是喂给模型的阶段清单，只给它做选择必需的信息。
type stageBrief struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

func briefs(stages []stage.Stage) []stageBrief {
	out := make([]stageBrief, 0, len(stages))
	for _, s := range stages {
		out = append(out, stageBrief{Key: s.Key, Label: s.Label, Kind: string(s.Kind)})
	}
	return out
}
