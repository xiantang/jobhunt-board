package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"interview/internal/ai"
	"interview/internal/application"
	"interview/internal/platform/apperr"
	"interview/internal/stage"
	"interview/internal/workflow"
)

// MaxTextLen 是允许粘贴的文本长度上限，按 rune 计。
// 邮件正文再长也够用，主要是挡住整封带签名档的转发链，省 token 也省钱。
const MaxTextLen = 20000

// StageProvider 提供看板的阶段配置与流转规则。
type StageProvider interface {
	List(ctx context.Context, boardID int64) ([]stage.Stage, error)
	FlowOf(ctx context.Context, boardID int64) (workflow.Flow, error)
}

// ApplicationService 是本模块用到的面试流程能力，全部是既有的公开方法——
// 确认落库不走任何旁路，重复公司名、跟进人要求、非法流转照样会被拦。
type ApplicationService interface {
	List(ctx context.Context, boardID int64, f application.Filter) ([]application.Application, error)
	Get(ctx context.Context, id int64) (application.Application, error)
	GetDetail(ctx context.Context, id int64) (application.Detail, error)
	Create(ctx context.Context, boardID int64, in application.CreateInput, actorID int64) (application.Application, error)
	Update(ctx context.Context, id int64, in application.UpdateInput, actorID int64) (application.Application, error)
	Move(ctx context.Context, id int64, toKey string, index int, actorID int64) (application.Application, error)
	ScheduleRound(ctx context.Context, applicationID int64, in application.RoundInput, actorID int64) (application.Round, error)
	UpdateRound(ctx context.Context, roundID int64, in application.RoundUpdateInput, actorID int64) (application.Round, error)
}

// Service 负责解析草稿与确认落库。
type Service struct {
	model  ai.Completer
	stages StageProvider
	apps   ApplicationService
	now    func() time.Time // 可替换，测试里固定时间
}

// NewService 构造 AI 录入服务。model 为 nil 表示没配 key，Parse 会返回 503。
func NewService(model ai.Completer, stages StageProvider, apps ApplicationService) *Service {
	return &Service{model: model, stages: stages, apps: apps, now: time.Now}
}

// Available 表示 AI 解析当前可用，前端据此决定要不要显示入口。
func (s *Service) Available() bool { return s.model != nil && s.model.Available() }

// Parse 把一段文本解析成草稿。它只读不写，任何结果都要用户确认后才落库。
func (s *Service) Parse(ctx context.Context, boardID int64, text string) (Draft, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Draft{}, apperr.Invalid("text", "请先粘贴要录入的文本")
	}
	if len([]rune(text)) > MaxTextLen {
		return Draft{}, apperr.Invalid("text", fmt.Sprintf("文本太长了，请控制在 %d 字以内", MaxTextLen))
	}
	if !s.Available() {
		return Draft{}, apperr.Unavailable(apperr.CodeAIUnavailable,
			"AI 录入未启用：服务端没有配置 OPENAI_API_KEY")
	}

	stages, err := s.stages.List(ctx, boardID)
	if err != nil {
		return Draft{}, err
	}
	if len(stages) == 0 {
		return Draft{}, apperr.Conflict(apperr.CodeConflict, "该看板还没有配置任何阶段，请先添加阶段")
	}

	raw, err := s.model.Complete(ctx, ai.Request{
		System:     systemPrompt(briefs(stages), s.now()),
		User:       text,
		SchemaName: "interview_ingest",
		Schema:     extractionSchema(),
	})
	if err != nil {
		if errors.Is(err, ai.ErrNotConfigured) {
			return Draft{}, apperr.Unavailable(apperr.CodeAIUnavailable,
				"AI 录入未启用：服务端没有配置 OPENAI_API_KEY")
		}
		return Draft{}, apperr.Unavailable(apperr.CodeAIUnavailable, "AI 解析失败："+err.Error())
	}

	var ex extraction
	if err := json.Unmarshal(raw, &ex); err != nil {
		return Draft{}, apperr.Unavailable(apperr.CodeAIUnavailable, "AI 返回的结果无法解析，请重试")
	}

	existing, err := s.apps.List(ctx, boardID, application.Filter{})
	if err != nil {
		return Draft{}, err
	}
	flow, err := s.stages.FlowOf(ctx, boardID)
	if err != nil {
		return Draft{}, err
	}
	return s.toDraft(ex, stages, existing, flow), nil
}

// toDraft 把模型输出收敛成草稿：阶段 key 必须真实存在，
// 公司名在本看板里找已有卡片，再算一次流转是否合法。
func (s *Service) toDraft(ex extraction, stages []stage.Stage, existing []application.Application, flow workflow.Flow) Draft {
	d := Draft{
		Company:      strings.TrimSpace(ex.Company),
		Role:         strings.TrimSpace(ex.Role),
		Channel:      strings.TrimSpace(ex.Channel),
		Notes:        strings.TrimSpace(ex.Notes),
		Summary:      strings.TrimSpace(ex.Summary),
		Confidence:   ex.Confidence,
		StageAllowed: true,
	}

	for _, st := range stages {
		if st.Key == ex.StageKey {
			d.StageKey, d.StageLabel = st.Key, st.Label
			break
		}
	}

	d.Round = DraftRound{
		Create:       true,
		ScheduledAt:  parseTime(ex.Round.ScheduledAt),
		DurationMin:  ex.Round.DurationMin,
		Mode:         ex.Round.Mode,
		MeetingURL:   strings.TrimSpace(ex.Round.MeetingURL),
		MeetingPlace: strings.TrimSpace(ex.Round.MeetingPlace),
		Interviewer:  strings.TrimSpace(ex.Round.Interviewer),
		Notes:        strings.TrimSpace(ex.Round.Notes),
		Deadline:     strings.TrimSpace(deref(ex.Round.Deadline)),
	}
	if d.Round.Deadline != "" {
		// 截止日期不是面试时间，折进备注里，免得被当成排期。
		d.Round.Notes = strings.TrimSpace("截止：" + d.Round.Deadline + "。" + d.Round.Notes)
	}

	d.Match = matchCompany(d.Company, existing)
	if d.Match != nil && d.StageKey != "" {
		from, errFrom := flow.Parse(d.Match.StageKey)
		to, errTo := flow.Parse(d.StageKey)
		if errFrom == nil && errTo == nil && !flow.Can(from, to) {
			d.StageAllowed = false
			d.StageWarning = fmt.Sprintf(
				"「%s」当前停在「%s」，不允许直接流转到「%s」。确认录入会保留原阶段，只加这一轮记录。",
				d.Match.Company, from.Label, to.Label)
		}
	}
	return d
}

// Confirm 把用户确认过的草稿写进看板：建卡或复用已有卡 → 流转 → 录入这一轮。
// 三步都调 application 的既有方法，规则校验与操作日志和手工操作完全一致。
func (s *Service) Confirm(ctx context.Context, boardID int64, in ConfirmInput, actorID int64) (Result, error) {
	var (
		res Result
		err error
	)

	if in.ApplicationID > 0 {
		res.Application, err = s.apps.Get(ctx, in.ApplicationID)
		if err != nil {
			return Result{}, err
		}
		if res.Application.BoardID != boardID {
			return Result{}, apperr.NotFound("这条流程不属于当前看板")
		}
		// 复用已有卡片时只补空缺，不覆盖用户已经填过的内容。
		if patch, ok := fillGaps(res.Application, in); ok {
			res.Application, err = s.apps.Update(ctx, res.Application.ID, patch, actorID)
			if err != nil {
				return Result{}, err
			}
		}
	} else {
		// 新卡片直接落在目标阶段：它没有历史，「从一面开始」是个起点而不是流转，
		// 不该被「HR 初筛 → 一面」这种相邻性检查挡住。
		res.Application, err = s.apps.Create(ctx, boardID, application.CreateInput{
			Company:  in.Company,
			Role:     in.Role,
			Channel:  in.Channel,
			Notes:    in.Notes,
			OwnerID:  in.OwnerID,
			Intent:   in.Intent,
			StageKey: in.StageKey,
		}, actorID)
		if err != nil {
			return Result{}, err
		}
		res.Created = true
	}

	if in.StageKey != "" && in.StageKey != res.Application.StageKey {
		moved, err := s.apps.Move(ctx, res.Application.ID, in.StageKey, -1, actorID)
		if err != nil {
			return Result{}, err
		}
		res.Application, res.Moved = moved, true
	}

	if in.Round.Create {
		round, err := s.recordRound(ctx, res.Application, in.Round, actorID)
		if err != nil {
			return Result{}, err
		}
		res.Round = &round
		// 建轮次会动 updated_at 与 next_round，重新读一次保证返回的是最新状态。
		if res.Application, err = s.apps.Get(ctx, res.Application.ID); err != nil {
			return Result{}, err
		}
	}
	return res, nil
}

// recordRound 录入这一轮。流转进面试阶段时系统会自动挂一条「待安排」，
// 这里优先把信息补到那一条上，避免同一个阶段出现两条记录。
func (s *Service) recordRound(ctx context.Context, app application.Application, in DraftRound, actorID int64) (application.Round, error) {
	input := application.RoundInput{
		ScheduledAt:  in.ScheduledAt,
		DurationMin:  in.DurationMin,
		Mode:         in.Mode,
		MeetingURL:   in.MeetingURL,
		MeetingPlace: in.MeetingPlace,
		Interviewer:  in.Interviewer,
		Notes:        in.Notes,
	}

	detail, err := s.apps.GetDetail(ctx, app.ID)
	if err != nil {
		return application.Round{}, err
	}
	for _, r := range detail.Rounds {
		if r.StageKey != app.StageKey || r.Result != application.ResultPending || r.ScheduledAt != nil {
			continue
		}
		if r.Interviewer != "" || r.MeetingURL != "" || r.MeetingPlace != "" || r.Notes != "" {
			continue // 已经有人填过内容，不覆盖
		}
		// 只覆盖真正解析出来的字段：UpdateRound 是「传了就改」的语义，
		// 把空值一股脑传过去会把自动建那条的默认时长清成 0。
		patch := application.RoundUpdateInput{ScheduledAt: input.ScheduledAt}
		if input.DurationMin > 0 {
			patch.DurationMin = &input.DurationMin
		}
		setIfFilled(&patch.Mode, input.Mode)
		setIfFilled(&patch.MeetingURL, input.MeetingURL)
		setIfFilled(&patch.MeetingPlace, input.MeetingPlace)
		setIfFilled(&patch.Interviewer, input.Interviewer)
		setIfFilled(&patch.Notes, input.Notes)
		return s.apps.UpdateRound(ctx, r.ID, patch, actorID)
	}
	return s.apps.ScheduleRound(ctx, app.ID, input, actorID)
}

// setIfFilled 把非空值挂到「传了就改」的可选字段上。
func setIfFilled(dst **string, v string) {
	if v != "" {
		*dst = &v
	}
}

// fillGaps 只把已有卡片上为空的字段补上，返回是否真的有东西要改。
func fillGaps(current application.Application, in ConfirmInput) (application.UpdateInput, bool) {
	var (
		patch   application.UpdateInput
		changed bool
	)
	fill := func(cur, next string, dst **string) {
		if cur != "" || strings.TrimSpace(next) == "" {
			return
		}
		v := strings.TrimSpace(next)
		*dst, changed = &v, true
	}
	fill(current.Role, in.Role, &patch.Role)
	fill(current.Channel, in.Channel, &patch.Channel)
	fill(current.Notes, in.Notes, &patch.Notes)
	if current.OwnerID == nil && in.OwnerID != nil && *in.OwnerID > 0 {
		patch.OwnerID, changed = in.OwnerID, true
	}
	return patch, changed
}

// matchCompany 在本看板已有卡片里找同一家公司。
// 先精确匹配规整后的名字，再退到包含关系（「foodpanda」对上「Foodpanda 台湾」）。
func matchCompany(company string, existing []application.Application) *application.Application {
	key := normalizeCompany(company)
	if key == "" {
		return nil
	}
	for i, a := range existing {
		if normalizeCompany(a.Company) == key {
			return &existing[i]
		}
	}
	if len([]rune(key)) < 3 {
		return nil // 太短的名字用包含匹配容易误伤
	}
	for i, a := range existing {
		other := normalizeCompany(a.Company)
		if other == "" {
			continue
		}
		if strings.Contains(other, key) || strings.Contains(key, other) {
			return &existing[i]
		}
	}
	return nil
}

// normalizeCompany 规整公司名用于比较：转小写、去掉空白与标点、抹掉常见后缀。
func normalizeCompany(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, suffix := range []string{"股份有限公司", "有限责任公司", "有限公司", "科技有限公司", "集团", "公司", " inc", " ltd", " llc", " corp"} {
		s = strings.ReplaceAll(s, suffix, "")
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseTime 解析模型给的时间。它偶尔会漏掉时区或只给到分钟，这里都兜住；
// 实在解析不出来就当作「没排期」，而不是塞一个错的时间进去。
func parseTime(raw *string) *time.Time {
	v := strings.TrimSpace(deref(raw))
	if v == "" {
		return nil
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, v, time.Local); err == nil {
			return &t
		}
	}
	return nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
