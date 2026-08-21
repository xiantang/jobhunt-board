package stage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"interview/internal/platform/apperr"
	"interview/internal/platform/ordering"
	"interview/internal/workflow"
)

// Service 承载阶段配置的读写。SQL 直接写在这里，模块很薄，不再单独拆 repo。
type Service struct {
	db *sql.DB
}

// NewService 构造阶段服务。
func NewService(db *sql.DB) *Service { return &Service{db: db} }

const selectStage = `
SELECT id, board_id, key, label, kind, color, requires_owner, position
FROM stages`

func scanStage(sc interface{ Scan(...any) error }) (Stage, error) {
	var (
		s        Stage
		kind     string
		requires int
	)
	if err := sc.Scan(&s.ID, &s.BoardID, &s.Key, &s.Label, &kind, &s.Color, &requires, &s.Position); err != nil {
		return Stage{}, err
	}
	s.Kind = workflow.Kind(kind)
	s.KindLabel = workflow.KindLabel(s.Kind)
	s.RequiresOwner = requires == 1
	return s, nil
}

// List 返回看板的全部阶段，按列顺序排列。
func (s *Service) List(ctx context.Context, boardID int64) ([]Stage, error) {
	rows, err := s.db.QueryContext(ctx, selectStage+` WHERE board_id = ? ORDER BY position, id`, boardID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()

	stages := make([]Stage, 0, 12)
	for rows.Next() {
		st, err := scanStage(rows)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		stages = append(stages, st)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	return stages, nil
}

// FlowOf 直接返回看板的流转规则，是 application 模块最常用的入口。
func (s *Service) FlowOf(ctx context.Context, boardID int64) (workflow.Flow, error) {
	stages, err := s.List(ctx, boardID)
	if err != nil {
		return workflow.Flow{}, err
	}
	return Flow(stages), nil
}

// Get 按 ID 查阶段。
func (s *Service) Get(ctx context.Context, id int64) (Stage, error) {
	st, err := scanStage(s.db.QueryRowContext(ctx, selectStage+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Stage{}, apperr.NotFound("阶段不存在")
	}
	if err != nil {
		return Stage{}, apperr.Internal(err)
	}
	return st, nil
}

// Create 新增一个阶段。key 由展示名生成并保证看板内唯一；
// Index < 0 时默认插到第一个终态阶段之前，符合「加一轮面试」的直觉。
func (s *Service) Create(ctx context.Context, boardID int64, in CreateInput) (Stage, error) {
	label, err := normalizeLabel(in.Label)
	if err != nil {
		return Stage{}, err
	}
	kind := workflow.KindNormal
	if in.Kind != "" {
		if kind, err = workflow.ParseKind(in.Kind); err != nil {
			return Stage{}, err
		}
	}

	existing, err := s.List(ctx, boardID)
	if err != nil {
		return Stage{}, err
	}
	if len(existing) >= 24 {
		return Stage{}, apperr.Conflict(apperr.CodeConflict, "一个看板最多配置 24 个阶段")
	}

	// 默认位置：普通/面试阶段插到第一个终态之前（「再加一轮面试」的直觉），
	// 终态自己则追加到最后。
	index := in.Index
	if index < 0 {
		index = len(existing)
		if !kind.Terminal() {
			for i, st := range existing {
				if st.Kind.Terminal() {
					index = i
					break
				}
			}
		}
	}
	positions := make([]float64, 0, len(existing))
	for _, st := range existing {
		positions = append(positions, st.Position)
	}

	color := strings.TrimSpace(in.Color)
	if color == "" {
		color = palette[len(existing)%len(palette)]
	}

	key := uniqueKey(slug(label), existing)
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO stages (board_id, key, label, kind, color, requires_owner, position, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		boardID, key, label, string(kind), color, boolToInt(in.RequiresOwner),
		ordering.At(positions, index), now)
	if err != nil {
		return Stage{}, apperr.Internal(err)
	}
	id, _ := res.LastInsertId()
	return s.Get(ctx, id)
}

// Update 修改阶段的展示名 / 类型 / 颜色 / 是否要求跟进人。key 始终不变，
// 所以改名不会影响已经落在该阶段的卡片。
func (s *Service) Update(ctx context.Context, id int64, in UpdateInput) (Stage, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Stage{}, err
	}

	sets := make([]string, 0, 4)
	args := make([]any, 0, 5)

	if in.Label != nil {
		label, err := normalizeLabel(*in.Label)
		if err != nil {
			return Stage{}, err
		}
		if label != current.Label {
			sets = append(sets, "label = ?")
			args = append(args, label)
		}
	}
	if in.Kind != nil {
		kind, err := workflow.ParseKind(*in.Kind)
		if err != nil {
			return Stage{}, err
		}
		if kind != current.Kind {
			// 把最后一个非终态阶段改成终态，会让新投递没有落脚点。
			if kind.Terminal() {
				if err := s.ensureNotLastActive(ctx, current, false); err != nil {
					return Stage{}, err
				}
			}
			sets = append(sets, "kind = ?")
			args = append(args, string(kind))
		}
	}
	if in.Color != nil {
		if color := strings.TrimSpace(*in.Color); color != "" && color != current.Color {
			sets = append(sets, "color = ?")
			args = append(args, color)
		}
	}
	if in.RequiresOwner != nil && *in.RequiresOwner != current.RequiresOwner {
		sets = append(sets, "requires_owner = ?")
		args = append(args, boolToInt(*in.RequiresOwner))
	}

	if len(sets) == 0 {
		return current, nil
	}
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx,
		"UPDATE stages SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return Stage{}, apperr.Internal(err)
	}
	return s.Get(ctx, id)
}

// Reorder 把阶段移动到看板的第 index 列。
func (s *Service) Reorder(ctx context.Context, id int64, index int) (Stage, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Stage{}, err
	}
	siblings, err := s.List(ctx, current.BoardID)
	if err != nil {
		return Stage{}, err
	}

	positions := make([]float64, 0, len(siblings))
	for _, st := range siblings {
		if st.ID != id {
			positions = append(positions, st.Position)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE stages SET position = ? WHERE id = ?`, ordering.At(positions, index), id); err != nil {
		return Stage{}, apperr.Internal(err)
	}
	return s.Get(ctx, id)
}

// Delete 删除阶段。阶段下还有卡片时拒绝，避免卡片凭空消失；
// 同时保证看板上至少留一个非终态阶段，否则新投递没有落脚点。
func (s *Service) Delete(ctx context.Context, id int64) error {
	current, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	var used int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM applications WHERE board_id = ? AND stage_key = ?`,
		current.BoardID, current.Key).Scan(&used); err != nil {
		return apperr.Internal(err)
	}
	if used > 0 {
		return apperr.Conflict(apperr.CodeConflict,
			fmt.Sprintf("「%s」下还有 %d 条流程，请先移走再删除", current.Label, used))
	}
	if err := s.ensureNotLastActive(ctx, current, true); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM stages WHERE id = ?`, id); err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// ensureNotLastActive 拦住「把最后一个非终态阶段删掉 / 改成终态」的操作。
func (s *Service) ensureNotLastActive(ctx context.Context, current Stage, removing bool) error {
	if current.Kind.Terminal() {
		return nil // 本来就是终态，删掉它不影响落脚点
	}
	siblings, err := s.List(ctx, current.BoardID)
	if err != nil {
		return err
	}
	for _, st := range siblings {
		if st.ID != current.ID && !st.Kind.Terminal() {
			return nil
		}
	}
	action := "改成终态"
	if removing {
		action = "删除"
	}
	return apperr.Conflict(apperr.CodeConflict,
		"看板至少要保留一个非终态阶段，不能"+action+"「"+current.Label+"」")
}

// uniqueKey 在看板内为新阶段找一个不冲突的 key。
func uniqueKey(base string, existing []Stage) string {
	taken := make(map[string]bool, len(existing))
	for _, st := range existing {
		taken[st.Key] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

func normalizeLabel(raw string) (string, error) {
	label := strings.TrimSpace(raw)
	if label == "" {
		return "", apperr.Invalid("label", "阶段名称不能为空")
	}
	if len([]rune(label)) > 20 {
		return "", apperr.Invalid("label", "阶段名称不能超过 20 个字符")
	}
	return label, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
