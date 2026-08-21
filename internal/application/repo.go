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
       nr.id, nr.stage_key, nr.stage_label, nr.scheduled_at, nr.duration_min, nr.mode,
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
		rStageKey, rStageLabel        sql.NullString
		rScheduled                    sql.NullString
		rDuration                     sql.NullInt64
		rMode, rURL, rPlace           sql.NullString
		rInterviewer, rResult, rNotes sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.BoardID, &a.Seq, &a.BoardKey, &a.Company, &a.Role, &a.Channel, &a.Notes,
		&a.StageKey, &a.StageLabel, &stageKind, &a.Intent, &ownerID, &name, &color,
		&a.Position, &createdRaw, &updatedR, &a.RoundCount,
		&rID, &rStageKey, &rStageLabel, &rScheduled, &rDuration, &rMode,
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
SELECT id, application_id, stage_key, stage_label, scheduled_at, duration_min, mode,
       meeting_url, meeting_place, interviewer, result, notes, created_at, updated_at
FROM interview_rounds`

func scanRound(sc scanner) (Round, error) {
	var (
		r                    Round
		scheduled            sql.NullString
		createdRaw, updatedR string
	)
	if err := sc.Scan(&r.ID, &r.ApplicationID, &r.StageKey, &r.StageLabel, &scheduled, &r.DurationMin,
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
		INSERT INTO interview_rounds (application_id, stage_key, stage_label, scheduled_at, duration_min,
		                              mode, meeting_url, meeting_place, interviewer, result, notes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		applicationID, stageKey, stageLabel, nullTime(in.ScheduledAt), in.DurationMin, in.Mode,
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
