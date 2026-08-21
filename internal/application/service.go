package application

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"interview/internal/platform/apperr"
	"interview/internal/platform/ordering"
	"interview/internal/workflow"
)

// MemberChecker 是本模块对成员模块的最小依赖：只用来校验跟进人是否可用。
type MemberChecker interface {
	Exists(ctx context.Context, id int64) (bool, error)
}

// BoardChecker 是本模块对看板模块的最小依赖。
type BoardChecker interface {
	Exists(ctx context.Context, id int64) (bool, error)
}

// StageProvider 提供某个看板当前的阶段流转规则。
// 每次操作都重新读取，所以用户改完阶段配置立刻生效。
type StageProvider interface {
	FlowOf(ctx context.Context, boardID int64) (workflow.Flow, error)
}

// Service 承载面试流程的业务逻辑：创建、维护、跟进人分配、阶段流转与面试排期。
type Service struct {
	repo    *repo
	members MemberChecker
	boards  BoardChecker
	stages  StageProvider
}

// NewService 构造面试流程服务。
func NewService(db *sql.DB, members MemberChecker, boards BoardChecker, stages StageProvider) *Service {
	return &Service{repo: &repo{db: db}, members: members, boards: boards, stages: stages}
}

// CreateInput 是新建流程的入参。
type CreateInput struct {
	Company string
	Role    string
	Channel string
	Notes   string
	OwnerID *int64
	Intent  string
}

// UpdateInput 是维护流程信息的入参，nil 表示该字段不改。
type UpdateInput struct {
	Company *string
	Role    *string
	Channel *string
	Notes   *string
	OwnerID *int64 // 指向 0 表示取消跟进人
	Intent  *string
}

// RoundInput 是安排一场面试的入参。
type RoundInput struct {
	ScheduledAt  *time.Time
	DurationMin  int
	Mode         string
	MeetingURL   string
	MeetingPlace string
	Interviewer  string
	Result       string
	Notes        string
}

// RoundUpdateInput 是改期 / 补会议信息 / 回填结果的入参，nil 表示不改。
type RoundUpdateInput struct {
	ScheduledAt   *time.Time
	ClearSchedule bool // 显式把时间清空，退回「待安排」
	DurationMin   *int
	Mode          *string
	MeetingURL    *string
	MeetingPlace  *string
	Interviewer   *string
	Result        *string
	Notes         *string
}

// List 返回看板下的流程（可筛选）。
func (s *Service) List(ctx context.Context, boardID int64, f Filter) ([]Application, error) {
	return s.repo.list(ctx, boardID, f)
}

// Get 返回单条流程。
func (s *Service) Get(ctx context.Context, id int64) (Application, error) {
	return s.repo.get(ctx, id)
}

// GetDetail 返回流程详情、面试轮次与操作日志。
func (s *Service) GetDetail(ctx context.Context, id int64) (Detail, error) {
	a, err := s.repo.get(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	rounds, err := s.repo.rounds(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	events, err := s.repo.events(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	return Detail{Application: a, Rounds: rounds, Events: events}, nil
}

// Create 在看板下新建一条面试流程，默认落在第一个阶段的列尾。
func (s *Service) Create(ctx context.Context, boardID int64, in CreateInput, actorID int64) (Application, error) {
	company, err := normalizeCompany(in.Company)
	if err != nil {
		return Application{}, err
	}
	role, err := normalizeText("role", in.Role, 60)
	if err != nil {
		return Application{}, err
	}
	channel, err := normalizeText("channel", in.Channel, 30)
	if err != nil {
		return Application{}, err
	}
	notes, err := normalizeText("notes", in.Notes, 2000)
	if err != nil {
		return Application{}, err
	}
	intent, err := normalizeIntent(in.Intent)
	if err != nil {
		return Application{}, err
	}
	if ok, err := s.boards.Exists(ctx, boardID); err != nil {
		return Application{}, err
	} else if !ok {
		return Application{}, apperr.NotFound("看板不存在")
	}
	if err := s.checkDuplicate(ctx, boardID, company, role, 0); err != nil {
		return Application{}, err
	}
	if err := s.checkOwner(ctx, in.OwnerID); err != nil {
		return Application{}, err
	}

	flow, err := s.stages.FlowOf(ctx, boardID)
	if err != nil {
		return Application{}, err
	}
	entry, err := flow.Entry()
	if err != nil {
		return Application{}, err
	}
	if entry.RequiresOwner && (in.OwnerID == nil || *in.OwnerID == 0) {
		return Application{}, apperr.Conflict(apperr.CodeOwnerRequired,
			fmt.Sprintf("「%s」要求先指定跟进人", entry.Label))
	}

	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return Application{}, apperr.Internal(err)
	}
	defer tx.Rollback()

	seq, err := s.repo.nextSeq(ctx, tx, boardID)
	if err != nil {
		return Application{}, apperr.Internal(err)
	}
	positions, err := s.repo.columnPositions(ctx, tx, boardID, entry.Key, 0)
	if err != nil {
		return Application{}, apperr.Internal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var owner any
	if in.OwnerID != nil && *in.OwnerID > 0 {
		owner = *in.OwnerID
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO applications (board_id, seq, company, role, channel, notes, stage_key, intent, owner_id, position, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		boardID, seq, company, role, channel, notes, entry.Key, intent, owner,
		ordering.At(positions, -1), now, now)
	if err != nil {
		return Application{}, apperr.Internal(err)
	}
	id, _ := res.LastInsertId()

	if err := insertEvent(ctx, tx, id, actorID, EventCreated, "", entry.Key,
		"创建了流程，落在「"+entry.Label+"」"); err != nil {
		return Application{}, apperr.Internal(err)
	}
	if owner != nil {
		if err := insertEvent(ctx, tx, id, actorID, EventOwnerChanged, "", "", "在创建时指定了跟进人"); err != nil {
			return Application{}, apperr.Internal(err)
		}
	}
	// 第一个阶段本身就是面试阶段时，同样先挂一条待安排的记录。
	if entry.Kind == workflow.KindInterview {
		if _, err := insertRound(ctx, tx, id, defaultRound(), entry.Key, entry.Label); err != nil {
			return Application{}, apperr.Internal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Application{}, apperr.Internal(err)
	}
	return s.repo.get(ctx, id)
}

// Update 维护流程信息，包括改派跟进人；每项变化都会写操作日志。
func (s *Service) Update(ctx context.Context, id int64, in UpdateInput, actorID int64) (Application, error) {
	current, err := s.repo.get(ctx, id)
	if err != nil {
		return Application{}, err
	}

	sets := make([]string, 0, 6)
	args := make([]any, 0, 7)
	logs := make([]string, 0, 6) // 每条一句人话，落到操作日志

	company, role := current.Company, current.Role // 校验重复时用改完之后的值

	text := []struct {
		column, field, label string
		in                   *string
		current              string
		max                  int
	}{
		{"company", "company", "公司", in.Company, current.Company, 60},
		{"role", "role", "岗位", in.Role, current.Role, 60},
		{"channel", "channel", "投递渠道", in.Channel, current.Channel, 30},
		{"notes", "notes", "备注", in.Notes, current.Notes, 2000},
	}
	for _, f := range text {
		if f.in == nil {
			continue
		}
		var value string
		if f.column == "company" {
			if value, err = normalizeCompany(*f.in); err != nil {
				return Application{}, err
			}
		} else if value, err = normalizeText(f.field, *f.in, f.max); err != nil {
			return Application{}, err
		}
		switch f.column {
		case "company":
			company = value
		case "role":
			role = value
		}
		if value != f.current {
			sets = append(sets, f.column+" = ?")
			args = append(args, value)
			logs = append(logs, "更新了"+f.label)
		}
	}
	// 改名可能撞上另一张卡片，和新建走同一条规则。
	if company != current.Company || role != current.Role {
		if err := s.checkDuplicate(ctx, current.BoardID, company, role, id); err != nil {
			return Application{}, err
		}
	}

	if in.Intent != nil {
		intent, err := normalizeIntent(*in.Intent)
		if err != nil {
			return Application{}, err
		}
		if intent != current.Intent {
			sets = append(sets, "intent = ?")
			args = append(args, intent)
			logs = append(logs, "调整了意向度")
		}
	}

	ownerChanged := ""
	if in.OwnerID != nil {
		if err := s.checkOwner(ctx, in.OwnerID); err != nil {
			return Application{}, err
		}
		var next any
		if *in.OwnerID > 0 {
			next = *in.OwnerID
		}
		changed := (current.OwnerID == nil && next != nil) ||
			(current.OwnerID != nil && next == nil) ||
			(current.OwnerID != nil && next != nil && *current.OwnerID != *in.OwnerID)
		if changed {
			sets = append(sets, "owner_id = ?")
			args = append(args, next)
			if next == nil {
				ownerChanged = "取消了跟进人"
			} else {
				ownerChanged = "指定了跟进人"
			}
		}
	}

	if len(sets) == 0 {
		return current, nil // 无变化，直接返回，不写日志
	}

	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return Application{}, apperr.Internal(err)
	}
	defer tx.Rollback()

	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Format(time.RFC3339), id)
	if _, err := tx.ExecContext(ctx,
		"UPDATE applications SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return Application{}, apperr.Internal(err)
	}

	if ownerChanged != "" {
		if err := insertEvent(ctx, tx, id, actorID, EventOwnerChanged, "", "", ownerChanged); err != nil {
			return Application{}, apperr.Internal(err)
		}
	}
	for _, detail := range logs {
		if err := insertEvent(ctx, tx, id, actorID, EventUpdated, "", "", detail); err != nil {
			return Application{}, apperr.Internal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Application{}, apperr.Internal(err)
	}
	return s.repo.get(ctx, id)
}

// Move 把流程推进到另一个阶段（拖拽与按钮共用）。index >= 0 时同时决定列内位置。
// 目标是面试阶段且还没有对应的待进行记录时，顺手建一条「待安排」，提醒去填时间。
func (s *Service) Move(ctx context.Context, id int64, toKey string, index int, actorID int64) (Application, error) {
	current, err := s.repo.get(ctx, id)
	if err != nil {
		return Application{}, err
	}
	flow, err := s.stages.FlowOf(ctx, current.BoardID)
	if err != nil {
		return Application{}, err
	}
	from, err := flow.Parse(current.StageKey)
	if err != nil {
		return Application{}, err
	}
	to, err := flow.Parse(toKey)
	if err != nil {
		return Application{}, err
	}
	if err := flow.Validate(from, to, current.OwnerID != nil); err != nil {
		return Application{}, err
	}

	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return Application{}, apperr.Internal(err)
	}
	defer tx.Rollback()

	positions, err := s.repo.columnPositions(ctx, tx, current.BoardID, to.Key, id)
	if err != nil {
		return Application{}, apperr.Internal(err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE applications SET stage_key = ?, position = ?, updated_at = ? WHERE id = ?`,
		to.Key, ordering.At(positions, index), time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return Application{}, apperr.Internal(err)
	}

	// 同列内拖动只是排序，不记流转日志，也不重复建面试记录。
	if from.Key != to.Key {
		if err := insertEvent(ctx, tx, id, actorID, EventStageChanged, from.Key, to.Key,
			"将阶段从「"+from.Label+"」推进到「"+to.Label+"」"); err != nil {
			return Application{}, apperr.Internal(err)
		}
		if to.Kind == workflow.KindInterview {
			var pending int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM interview_rounds WHERE application_id = ? AND stage_key = ? AND result = 'pending'`,
				id, to.Key).Scan(&pending); err != nil {
				return Application{}, apperr.Internal(err)
			}
			if pending == 0 {
				if _, err := insertRound(ctx, tx, id, defaultRound(), to.Key, to.Label); err != nil {
					return Application{}, apperr.Internal(err)
				}
				if err := insertEvent(ctx, tx, id, actorID, EventRoundScheduled, "", "",
					"新建了一条待安排的「"+to.Label+"」面试"); err != nil {
					return Application{}, apperr.Internal(err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return Application{}, apperr.Internal(err)
	}
	return s.repo.get(ctx, id)
}

// ---------- 面试轮次 ----------

// ScheduleRound 为流程安排一场面试。阶段名取当前阶段并存快照。
func (s *Service) ScheduleRound(ctx context.Context, applicationID int64, in RoundInput, actorID int64) (Round, error) {
	current, err := s.repo.get(ctx, applicationID)
	if err != nil {
		return Round{}, err
	}
	normalized, err := normalizeRound(in)
	if err != nil {
		return Round{}, err
	}

	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return Round{}, apperr.Internal(err)
	}
	defer tx.Rollback()

	id, err := insertRound(ctx, tx, applicationID, normalized, current.StageKey, current.StageLabel)
	if err != nil {
		return Round{}, apperr.Internal(err)
	}
	if err := insertEvent(ctx, tx, applicationID, actorID, EventRoundScheduled, "", "",
		"安排了一场「"+current.StageLabel+"」面试"+whenSuffix(normalized.ScheduledAt)); err != nil {
		return Round{}, apperr.Internal(err)
	}
	if err := touch(ctx, tx, applicationID); err != nil {
		return Round{}, apperr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return Round{}, apperr.Internal(err)
	}
	return s.repo.getRound(ctx, id)
}

// UpdateRound 改期、补会议信息或回填面试结果。
func (s *Service) UpdateRound(ctx context.Context, roundID int64, in RoundUpdateInput, actorID int64) (Round, error) {
	current, err := s.repo.getRound(ctx, roundID)
	if err != nil {
		return Round{}, err
	}

	sets := make([]string, 0, 8)
	args := make([]any, 0, 9)
	logs := make([]string, 0, 3)

	switch {
	case in.ClearSchedule && current.ScheduledAt != nil:
		sets = append(sets, "scheduled_at = NULL")
		logs = append(logs, "取消了「"+current.StageLabel+"」的面试时间")
	case in.ScheduledAt != nil:
		if current.ScheduledAt == nil || !current.ScheduledAt.Equal(*in.ScheduledAt) {
			sets = append(sets, "scheduled_at = ?")
			args = append(args, in.ScheduledAt.UTC().Format(time.RFC3339))
			logs = append(logs, "把「"+current.StageLabel+"」面试定在"+whenText(in.ScheduledAt))
		}
	}

	if in.DurationMin != nil && *in.DurationMin != current.DurationMin {
		if *in.DurationMin < 5 || *in.DurationMin > 600 {
			return Round{}, apperr.Invalid("duration_min", "面试时长需要在 5~600 分钟之间")
		}
		sets = append(sets, "duration_min = ?")
		args = append(args, *in.DurationMin)
	}
	if in.Mode != nil {
		mode, err := normalizeMode(*in.Mode)
		if err != nil {
			return Round{}, err
		}
		if mode != current.Mode {
			sets = append(sets, "mode = ?")
			args = append(args, mode)
		}
	}

	meeting := []struct {
		column, field string
		in            *string
		current       string
		max           int
	}{
		{"meeting_url", "meeting_url", in.MeetingURL, current.MeetingURL, 300},
		{"meeting_place", "meeting_place", in.MeetingPlace, current.MeetingPlace, 120},
		{"interviewer", "interviewer", in.Interviewer, current.Interviewer, 60},
		{"notes", "notes", in.Notes, current.Notes, 2000},
	}
	meetingChanged := false
	for _, f := range meeting {
		if f.in == nil {
			continue
		}
		value, err := normalizeText(f.field, *f.in, f.max)
		if err != nil {
			return Round{}, err
		}
		if value != f.current {
			sets = append(sets, f.column+" = ?")
			args = append(args, value)
			if f.column == "meeting_url" || f.column == "meeting_place" {
				meetingChanged = true
			}
		}
	}
	if meetingChanged {
		logs = append(logs, "更新了「"+current.StageLabel+"」的会议信息")
	}

	if in.Result != nil {
		result, err := normalizeResult(*in.Result)
		if err != nil {
			return Round{}, err
		}
		if result != current.Result {
			sets = append(sets, "result = ?")
			args = append(args, result)
			logs = append(logs, "把「"+current.StageLabel+"」的结果记为「"+labelOr(resultLabels, result)+"」")
		}
	}

	if len(sets) == 0 {
		return current, nil
	}

	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return Round{}, apperr.Internal(err)
	}
	defer tx.Rollback()

	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Format(time.RFC3339), roundID)
	if _, err := tx.ExecContext(ctx,
		"UPDATE interview_rounds SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return Round{}, apperr.Internal(err)
	}
	for _, detail := range logs {
		if err := insertEvent(ctx, tx, current.ApplicationID, actorID, EventRoundUpdated, "", "", detail); err != nil {
			return Round{}, apperr.Internal(err)
		}
	}
	if err := touch(ctx, tx, current.ApplicationID); err != nil {
		return Round{}, apperr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return Round{}, apperr.Internal(err)
	}
	return s.repo.getRound(ctx, roundID)
}

// DeleteRound 删除一条面试记录，返回它所属的流程 ID。
func (s *Service) DeleteRound(ctx context.Context, roundID, actorID int64) (int64, error) {
	current, err := s.repo.getRound(ctx, roundID)
	if err != nil {
		return 0, err
	}

	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, apperr.Internal(err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM interview_rounds WHERE id = ?`, roundID); err != nil {
		return 0, apperr.Internal(err)
	}
	if err := insertEvent(ctx, tx, current.ApplicationID, actorID, EventRoundRemoved, "", "",
		"删除了一条「"+current.StageLabel+"」面试记录"); err != nil {
		return 0, apperr.Internal(err)
	}
	if err := touch(ctx, tx, current.ApplicationID); err != nil {
		return 0, apperr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, apperr.Internal(err)
	}
	return current.ApplicationID, nil
}

// Delete 删除一条面试流程。它的面试记录与操作日志通过外键
// ON DELETE CASCADE 一并清除，不再单独记事件（日志随主行一起消失）。
func (s *Service) Delete(ctx context.Context, id int64) error {
	if _, err := s.repo.get(ctx, id); err != nil {
		return err
	}
	if _, err := s.repo.db.ExecContext(ctx, `DELETE FROM applications WHERE id = ?`, id); err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// ---------- 校验与小工具 ----------

// checkDuplicate 保证同一看板里「公司 + 岗位」只出现一张卡片，
// 免得同一家公司散成多张卡各自流转，看不出真实进度。
// 只拦新增与改名，存量重复数据保持原样。
func (s *Service) checkDuplicate(ctx context.Context, boardID int64, company, role string, excludeID int64) error {
	key, found, err := s.repo.findDuplicate(ctx, boardID, company, role, excludeID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	what := "「" + company + "」"
	if role != "" {
		what += "的「" + role + "」"
	}
	return apperr.Conflict(apperr.CodeConflict, what+"已经有一张卡片了（"+key+"），请直接用那张")
}

// checkOwner 校验跟进人存在；nil 或 0 表示不指定。
func (s *Service) checkOwner(ctx context.Context, id *int64) error {
	if id == nil || *id == 0 {
		return nil
	}
	if *id < 0 {
		return apperr.Invalid("owner_id", "跟进人 ID 不合法")
	}
	ok, err := s.members.Exists(ctx, *id)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.NotFound(fmt.Sprintf("跟进人不存在（id=%d）", *id))
	}
	return nil
}

// defaultRound 是自动创建的「待安排」面试记录。
func defaultRound() RoundInput {
	return RoundInput{DurationMin: 60, Mode: ModeOnline, Result: ResultPending}
}

func normalizeRound(in RoundInput) (RoundInput, error) {
	out := in
	if out.DurationMin == 0 {
		out.DurationMin = 60
	}
	if out.DurationMin < 5 || out.DurationMin > 600 {
		return RoundInput{}, apperr.Invalid("duration_min", "面试时长需要在 5~600 分钟之间")
	}
	mode, err := normalizeMode(out.Mode)
	if err != nil {
		return RoundInput{}, err
	}
	out.Mode = mode
	result, err := normalizeResult(out.Result)
	if err != nil {
		return RoundInput{}, err
	}
	out.Result = result

	if out.MeetingURL, err = normalizeText("meeting_url", out.MeetingURL, 300); err != nil {
		return RoundInput{}, err
	}
	if out.MeetingPlace, err = normalizeText("meeting_place", out.MeetingPlace, 120); err != nil {
		return RoundInput{}, err
	}
	if out.Interviewer, err = normalizeText("interviewer", out.Interviewer, 60); err != nil {
		return RoundInput{}, err
	}
	if out.Notes, err = normalizeText("notes", out.Notes, 2000); err != nil {
		return RoundInput{}, err
	}
	return out, nil
}

func normalizeIntent(raw string) (string, error) {
	switch raw {
	case "":
		return IntentNormal, nil
	case IntentLow, IntentNormal, IntentHigh:
		return raw, nil
	default:
		return "", apperr.Invalid("intent", "意向度只能是 low、normal 或 high")
	}
}

func normalizeMode(raw string) (string, error) {
	switch raw {
	case "":
		return ModeOnline, nil
	case ModeOnline, ModeOnsite, ModePhone:
		return raw, nil
	default:
		return "", apperr.Invalid("mode", "面试方式只能是 online、onsite 或 phone")
	}
}

func normalizeResult(raw string) (string, error) {
	switch raw {
	case "":
		return ResultPending, nil
	case ResultPending, ResultPassed, ResultFailed, ResultCancelled:
		return raw, nil
	default:
		return "", apperr.Invalid("result", "面试结果只能是 pending、passed、failed 或 cancelled")
	}
}

// touch 刷新流程的 updated_at，让排期变更也体现在卡片上。
func touch(ctx context.Context, tx *sql.Tx, applicationID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE applications SET updated_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), applicationID)
	return err
}

func whenText(t *time.Time) string {
	if t == nil {
		return "待定"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func whenSuffix(t *time.Time) string {
	if t == nil {
		return "（待安排时间）"
	}
	return "，时间 " + whenText(t)
}
