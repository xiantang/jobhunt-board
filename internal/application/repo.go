package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"interview/internal/platform/apperr"
	"interview/internal/workflow"
)

// repo 封装流程、面试轮次与操作日志的 SQL 访问，service 之外不暴露。
type repo struct {
	db *sql.DB
}

// selectApplication 统一的查询投影：
// 连 stages 拿到当前阶段的展示名与类型，连 members 拿到跟进人，
// 再用一个子查询挑出「下一场待进行的面试」——已排期的优先，未排期的排在后面。
const selectApplication = `
SELECT a.id, a.board_id, a.seq, b.key, a.company, a.role, a.channel, a.notes,
       a.stage_key, COALESCE(st.label, a.stage_key), COALESCE(st.kind, 'normal'),
       a.intent, a.owner_id, m.name, m.color, a.position, a.created_at, a.updated_at,
       (SELECT COUNT(*) FROM interview_rounds cr WHERE cr.application_id = a.id),
       nr.id, nr.stage_key, nr.stage_label, nr.kind, nr.scheduled_at, nr.duration_min, nr.mode,
       nr.meeting_url, nr.meeting_place, nr.interviewer, nr.result, nr.notes
FROM applications a
JOIN boards b ON b.id = a.board_id
LEFT JOIN stages st ON st.board_id = a.board_id AND st.key = a.stage_key
LEFT JOIN members m ON m.id = a.owner_id
LEFT JOIN interview_rounds nr ON nr.id = (
    SELECT r2.id FROM interview_rounds r2
    WHERE r2.application_id = a.id AND r2.result = 'pending'
    ORDER BY (r2.scheduled_at IS NULL), r2.scheduled_at, r2.id
    LIMIT 1
)`

// scanner 兼容 *sql.Row 与 *sql.Rows。
type scanner interface {
	Scan(dest ...any) error
}

func scanApplication(sc scanner) (Application, error) {
	var (
		a                    Application
		ownerID              sql.NullInt64
		name, color          sql.NullString
		stageKind            string
		createdRaw, updatedR string

		rID                           sql.NullInt64
		rStageKey, rStageLabel, rKind sql.NullString
		rScheduled                    sql.NullString
		rDuration                     sql.NullInt64
		rMode, rURL, rPlace           sql.NullString
		rInterviewer, rResult, rNotes sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.BoardID, &a.Seq, &a.BoardKey, &a.Company, &a.Role, &a.Channel, &a.Notes,
		&a.StageKey, &a.StageLabel, &stageKind, &a.Intent, &ownerID, &name, &color,
		&a.Position, &createdRaw, &updatedR, &a.RoundCount,
		&rID, &rStageKey, &rStageLabel, &rKind, &rScheduled, &rDuration, &rMode,
		&rURL, &rPlace, &rInterviewer, &rResult, &rNotes); err != nil {
		return Application{}, err
	}

	a.StageKind = workflow.Kind(stageKind)
	a.Key = fmt.Sprintf("%s-%d", a.BoardKey, a.Seq)
	if ownerID.Valid {
		id := ownerID.Int64
		a.OwnerID = &id
		a.Owner = &Owner{ID: id, Name: name.String, Color: color.String}
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339, createdRaw)
	a.UpdatedAt, _ = time.Parse(time.RFC3339, updatedR)

	if rID.Valid {
		a.NextRound = &Round{
			ID:            rID.Int64,
			ApplicationID: a.ID,
			StageKey:      rStageKey.String,
			StageLabel:    rStageLabel.String,
			Kind:          rKind.String,
			ScheduledAt:   parseNullTime(rScheduled),
			DurationMin:   int(rDuration.Int64),
			Mode:          rMode.String,
			MeetingURL:    rURL.String,
			MeetingPlace:  rPlace.String,
			Interviewer:   rInterviewer.String,
			Result:        rResult.String,
			Notes:         rNotes.String,
		}
	}
	return a, nil
}

// list 按看板 + 筛选条件返回流程，按阶段列与列内位置排序。
func (r *repo) list(ctx context.Context, boardID int64, f Filter) ([]Application, error) {
	query := selectApplication + ` WHERE a.board_id = ?`
	args := []any{boardID}

	if f.OwnerID != nil {
		if *f.OwnerID == 0 { // 0 表示「未分配」
			query += ` AND a.owner_id IS NULL`
		} else {
			query += ` AND a.owner_id = ?`
			args = append(args, *f.OwnerID)
		}
	}
	if f.StageKey != nil {
		query += ` AND a.stage_key = ?`
		args = append(args, *f.StageKey)
	}
	if f.Upcoming {
		query += ` AND nr.id IS NOT NULL AND nr.scheduled_at IS NOT NULL`
	}
	query += ` ORDER BY a.position, a.id`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()

	items := make([]Application, 0, 16)
	for rows.Next() {
		a, err := scanApplication(rows)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	return items, nil
}

// get 按 ID 查流程。
func (r *repo) get(ctx context.Context, id int64) (Application, error) {
	a, err := scanApplication(r.db.QueryRowContext(ctx, selectApplication+` WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Application{}, apperr.NotFound("面试流程不存在")
	}
	if err != nil {
		return Application{}, apperr.Internal(err)
	}
	return a, nil
}

// findDuplicate 查同一看板里公司 + 岗位都相同的另一张卡片，返回它的卡片编号。
// excludeID 用来在改名时排除自己。用 NOCASE 比较，免得
// 「ByteDance」和「bytedance」被当成两家公司各占一张卡。
func (r *repo) findDuplicate(ctx context.Context, boardID int64, company, role string, excludeID int64) (string, bool, error) {
	var (
		boardKey string
		seq      int
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT b.key, a.seq FROM applications a
		JOIN boards b ON b.id = a.board_id
		WHERE a.board_id = ? AND a.id <> ?
		  AND a.company = ? COLLATE NOCASE
		  AND a.role = ? COLLATE NOCASE
		ORDER BY a.id LIMIT 1`, boardID, excludeID, company, role).Scan(&boardKey, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, apperr.Internal(err)
	}
	return fmt.Sprintf("%s-%d", boardKey, seq), true, nil
}

// nextSeq 返回看板内下一个流程编号。
func (r *repo) nextSeq(ctx context.Context, tx *sql.Tx, boardID int64) (int, error) {
	var seq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM applications WHERE board_id = ?`, boardID).Scan(&seq); err != nil {
		return 0, err
	}
	return int(seq.Int64) + 1, nil
}

// columnPositions 返回某一列现有卡片的位置（可排除指定卡片），用于计算插入点。
func (r *repo) columnPositions(ctx context.Context, tx *sql.Tx, boardID int64, stageKey string, excludeID int64) ([]float64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT position FROM applications WHERE board_id = ? AND stage_key = ? AND id <> ? ORDER BY position, id`,
		boardID, stageKey, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	positions := make([]float64, 0, 16)
	for rows.Next() {
		var p float64
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

// ---------- 面试轮次 ----------

const selectRound = `
SELECT id, application_id, stage_key, stage_label, kind, scheduled_at, duration_min, mode,
       meeting_url, meeting_place, interviewer, result, notes, created_at, updated_at
FROM interview_rounds`

func scanRound(sc scanner) (Round, error) {
	var (
		r                    Round
		scheduled            sql.NullString
		createdRaw, updatedR string
	)
	if err := sc.Scan(&r.ID, &r.ApplicationID, &r.StageKey, &r.StageLabel, &r.Kind, &scheduled, &r.DurationMin,
		&r.Mode, &r.MeetingURL, &r.MeetingPlace, &r.Interviewer, &r.Result, &r.Notes,
		&createdRaw, &updatedR); err != nil {
		return Round{}, err
	}
	r.ScheduledAt = parseNullTime(scheduled)
	r.CreatedAt, _ = time.Parse(time.RFC3339, createdRaw)
	r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedR)
	return r, nil
}

// rounds 返回一条流程的全部面试轮次：已排期的按时间正序，未排期的排在最后。
func (r *repo) rounds(ctx context.Context, applicationID int64) ([]Round, error) {
	rows, err := r.db.QueryContext(ctx,
		selectRound+` WHERE application_id = ? ORDER BY (scheduled_at IS NULL), scheduled_at, id`,
		applicationID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()

	out := make([]Round, 0, 8)
	for rows.Next() {
		round, err := scanRound(rows)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		out = append(out, round)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	return out, nil
}

// getRound 按 ID 查一轮面试。
func (r *repo) getRound(ctx context.Context, id int64) (Round, error) {
	round, err := scanRound(r.db.QueryRowContext(ctx, selectRound+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Round{}, apperr.NotFound("面试记录不存在")
	}
	if err != nil {
		return Round{}, apperr.Internal(err)
	}
	return round, nil
}

// insertRound 写一条面试轮次，scheduledAt 为空表示待安排。
func insertRound(ctx context.Context, tx *sql.Tx, applicationID int64, in RoundInput, stageKey, stageLabel string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO interview_rounds (application_id, stage_key, stage_label, kind, scheduled_at, duration_min,
		                              mode, meeting_url, meeting_place, interviewer, result, notes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		applicationID, stageKey, stageLabel, in.Kind, nullTime(in.ScheduledAt), in.DurationMin, in.Mode,
		in.MeetingURL, in.MeetingPlace, in.Interviewer, in.Result, in.Notes, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ---------- 操作日志 ----------

// insertEvent 写一条操作日志。
func insertEvent(ctx context.Context, tx *sql.Tx, applicationID, actorID int64, typ, from, to, detail string) error {
	var actor any
	if actorID > 0 {
		actor = actorID
	}
	var fromVal, toVal any
	if from != "" {
		fromVal = from
	}
	if to != "" {
		toVal = to
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO application_events (application_id, actor_id, type, from_stage, to_stage, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		applicationID, actor, typ, fromVal, toVal, detail, time.Now().UTC().Format(time.RFC3339))
	return err
}

// events 返回流程的操作日志，最新在前。
func (r *repo) events(ctx context.Context, applicationID int64) ([]Event, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.type, COALESCE(m.name, ''), COALESCE(e.from_stage, ''), COALESCE(e.to_stage, ''),
		       e.detail, e.created_at
		FROM application_events e
		LEFT JOIN members m ON m.id = e.actor_id
		WHERE e.application_id = ?
		ORDER BY e.id DESC`, applicationID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()

	events := make([]Event, 0, 8)
	for rows.Next() {
		var (
			e   Event
			raw string
		)
		if err := rows.Scan(&e.ID, &e.Type, &e.ActorName, &e.FromStage, &e.ToStage, &e.Detail, &raw); err != nil {
			return nil, apperr.Internal(err)
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, raw)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	return events, nil
}

// ---------- 小工具 ----------

func parseNullTime(raw sql.NullString) *time.Time {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, raw.String)
	if err != nil {
		return nil
	}
	return &t
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// normalizeCompany 去掉首尾空白，并校验长度。
func normalizeCompany(raw string) (string, error) {
	company := strings.TrimSpace(raw)
	if company == "" {
		return "", apperr.Invalid("company", "公司名称不能为空")
	}
	if len([]rune(company)) > 60 {
		return "", apperr.Invalid("company", "公司名称不能超过 60 个字符")
	}
	return company, nil
}

// normalizeText 校验可选文本字段的长度。
func normalizeText(field, raw string, max int) (string, error) {
	text := strings.TrimSpace(raw)
	if len([]rune(text)) > max {
		return "", apperr.Invalid(field, fmt.Sprintf("内容不能超过 %d 个字符", max))
	}
	return text, nil
}

// ---------- 日程页用的跨看板查询 ----------

// selectScheduled 把轮次、流程、看板、阶段配色 join 到一起。
// 阶段可能已经被删掉（历史轮次还在），所以 stages 用 LEFT JOIN，
// 拿不到配色就退回灰色，而不是让整条记录消失。
const selectScheduled = `
SELECT r.id, r.application_id, b.key, a.seq, a.company, a.role,
       r.stage_key, r.stage_label, COALESCE(s.color, '#6b7280'), r.kind,
       r.scheduled_at, r.duration_min, r.mode, r.meeting_url, r.meeting_place,
       r.interviewer, r.result, r.notes, r.google_event_id
FROM interview_rounds r
JOIN applications a ON a.id = r.application_id
JOIN boards       b ON b.id = a.board_id
LEFT JOIN stages  s ON s.board_id = a.board_id AND s.key = r.stage_key`

func scanScheduled(sc interface{ Scan(...any) error }) (ScheduledRound, error) {
	var (
		r         ScheduledRound
		seq       int
		boardKey  string
		scheduled sql.NullString
	)
	if err := sc.Scan(&r.RoundID, &r.ApplicationID, &boardKey, &seq, &r.Company, &r.Role,
		&r.StageKey, &r.StageLabel, &r.StageColor, &r.Kind, &scheduled, &r.DurationMin, &r.Mode,
		&r.MeetingURL, &r.MeetingPlace, &r.Interviewer, &r.Result, &r.Notes,
		&r.GoogleEventID); err != nil {
		return ScheduledRound{}, err
	}
	r.BoardKey = boardKey
	r.ApplicationKey = fmt.Sprintf("%s-%d", boardKey, seq)
	if scheduled.Valid {
		if t, err := time.Parse(time.RFC3339, scheduled.String); err == nil {
			local := t.Local()
			r.ScheduledAt = &local
		}
	}
	return r, nil
}

func (r *repo) scheduledRounds(ctx context.Context, query string, args ...any) ([]ScheduledRound, error) {
	rows, err := r.db.QueryContext(ctx, selectScheduled+query, args...)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()

	out := make([]ScheduledRound, 0, 16)
	for rows.Next() {
		item, err := scanScheduled(rows)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	return out, nil
}

// upcomingRounds 返回 [from, to) 内已排期的面试。
// 已取消的不出现在日程上；已通过/未通过的留着，方便回看这一周都发生了什么。
func (r *repo) upcomingRounds(ctx context.Context, from, to time.Time) ([]ScheduledRound, error) {
	return r.scheduledRounds(ctx, `
		WHERE r.scheduled_at IS NOT NULL
		  AND r.scheduled_at >= ? AND r.scheduled_at < ?
		  AND r.result != 'cancelled'
		ORDER BY r.scheduled_at`,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
}

// unscheduledRounds 返回还没定时间、并且还有可能发生的面试。
//
// 卡片已经走到终态（拿到 Offer 或已结束）时，它下面挂着的待安排轮次
// 不会再发生了——那是流程中断留下的残影，不该出现在「还没定时间」里催人去约。
// 卡片当前阶段用 LEFT JOIN 取：阶段被删掉时 kind 为 NULL，按「还在推进」处理，
// 宁可多显示一条，也不要把还要跟的面试藏起来。
func (r *repo) unscheduledRounds(ctx context.Context) ([]ScheduledRound, error) {
	return r.scheduledRounds(ctx, `
		LEFT JOIN stages cur ON cur.board_id = a.board_id AND cur.key = a.stage_key
		WHERE r.scheduled_at IS NULL AND r.result = 'pending'
		  AND (cur.kind IS NULL OR cur.kind NOT IN (?, ?))
		ORDER BY a.updated_at DESC`,
		string(workflow.KindOffer), string(workflow.KindRejected))
}

// scheduledRound 按轮次 id 取一条，用来拼日程标题。
func (r *repo) scheduledRound(ctx context.Context, roundID int64) (ScheduledRound, error) {
	item, err := scanScheduled(r.db.QueryRowContext(ctx, selectScheduled+` WHERE r.id = ?`, roundID))
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduledRound{}, apperr.NotFound("面试记录不存在")
	}
	if err != nil {
		return ScheduledRound{}, apperr.Internal(err)
	}
	return item, nil
}

// setGoogleEventID 记下这一轮对应的 Google 事件 id。
func (r *repo) setGoogleEventID(ctx context.Context, roundID int64, eventID string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE interview_rounds SET google_event_id = ? WHERE id = ?`, eventID, roundID); err != nil {
		return apperr.Internal(err)
	}
	return nil
}
