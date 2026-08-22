package api

import (
	"github.com/gin-gonic/gin"

	"interview/internal/ingest"
	"interview/internal/platform/apperr"
	"interview/internal/platform/ginx"
)

// ParseIngestReq 是 AI 解析的请求体：一段随手粘贴的文本。
type ParseIngestReq struct {
	Text string `json:"text" binding:"required"`
}

// ParseIngest 把文本解析成草稿。它只读不写——落库要再调一次 confirm。
func (h *Handler) ParseIngest(c *gin.Context) {
	if h.Ingest == nil {
		ginx.Fail(c, apperr.Unavailable(apperr.CodeAIUnavailable, "AI 录入未启用"))
		return
	}
	var req ParseIngestReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	b, err := h.Boards.GetByKey(c.Request.Context(), c.Param("key"))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	draft, err := h.Ingest.Parse(c.Request.Context(), b.ID, req.Text)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.OK(c, gin.H{"draft": draft})
}

// ConfirmIngestReq 是确认落库的请求体。
// 字段都是用户在草稿上改过的，模型到这一步已经不参与了。
type ConfirmIngestReq struct {
	ApplicationID int64  `json:"application_id"` // > 0 表示录到已有卡片上
	Company       string `json:"company"  binding:"max=60"`
	Role          string `json:"role"     binding:"max=60"`
	Channel       string `json:"channel"  binding:"max=30"`
	Notes         string `json:"notes"    binding:"max=2000"`
	OwnerID       *int64 `json:"owner_id"`
	Intent        string `json:"intent"    binding:"omitempty,oneof=low normal high"`
	StageKey      string `json:"stage_key"`

	CreateRound  bool    `json:"create_round"`
	ScheduledAt  *string `json:"scheduled_at"`
	DurationMin  int     `json:"duration_min"  binding:"omitempty,min=5,max=600"`
	Mode         string  `json:"mode"          binding:"omitempty,oneof=online onsite phone"`
	MeetingURL   string  `json:"meeting_url"   binding:"max=300"`
	MeetingPlace string  `json:"meeting_place" binding:"max=120"`
	Interviewer  string  `json:"interviewer"   binding:"max=60"`
	RoundNotes   string  `json:"round_notes"   binding:"max=2000"`
}

// ConfirmIngest 把确认后的草稿写进看板：建卡 / 复用已有卡 → 流转 → 录入这一轮。
func (h *Handler) ConfirmIngest(c *gin.Context) {
	if h.Ingest == nil {
		ginx.Fail(c, apperr.Unavailable(apperr.CodeAIUnavailable, "AI 录入未启用"))
		return
	}
	var req ConfirmIngestReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	b, err := h.Boards.GetByKey(c.Request.Context(), c.Param("key"))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	at, _, err := parseSchedule(req.ScheduledAt)
	if err != nil {
		ginx.Fail(c, err)
		return
	}

	res, err := h.Ingest.Confirm(c.Request.Context(), b.ID, ingest.ConfirmInput{
		ApplicationID: req.ApplicationID,
		Company:       req.Company,
		Role:          req.Role,
		Channel:       req.Channel,
		Notes:         req.Notes,
		OwnerID:       req.OwnerID,
		Intent:        req.Intent,
		StageKey:      req.StageKey,
		Round: ingest.DraftRound{
			Create:       req.CreateRound,
			ScheduledAt:  at,
			DurationMin:  req.DurationMin,
			Mode:         req.Mode,
			MeetingURL:   req.MeetingURL,
			MeetingPlace: req.MeetingPlace,
			Interviewer:  req.Interviewer,
			Notes:        req.RoundNotes,
		},
	}, ginx.ActorID(c))
	if err != nil {
		ginx.Fail(c, err)
		return
	}

	payload := gin.H{"application": res.Application, "created": res.Created, "moved": res.Moved}
	if res.Round != nil {
		payload["round"] = res.Round
		h.autoSync(c, res.Round.ID, payload)
	}
	h.respondWithBoard(c, res.Application.BoardKey, payload, res.Created)
}
