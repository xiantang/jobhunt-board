// Package board 是看板模块：承载一套阶段配置与其下的全部面试流程，
// 并给出整体进展摘要。
package board

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"interview/internal/platform/apperr"
	"interview/internal/workflow"
)

// DefaultKey 是种子看板的 key，首页直接跳转到它。
const DefaultKey = "JOBHUNT"

// Board 是看板基本信息。
type Board struct {
	ID          int64  `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Summary 是看板进展摘要。
type Summary struct {
	Total    int `json:"total"`    // 全部流程
	Active   int `json:"active"`   // 还在推进中（非终态）
	Offer    int `json:"offer"`    // 已拿到 Offer
	Rejected int `json:"rejected"` // 已结束 / 挂
	Upcoming int `json:"upcoming"` // 未来 7 天内已排期的面试场次
	Percent  int `json:"percent"`  // 已出结果占比
}

// Service 承载看板查询与进展统计。
type Service struct {
	db *sql.DB
}

// NewService 构造看板服务。
func NewService(db *sql.DB) *Service { return &Service{db: db} }

// GetByKey 按 key 查看板，不存在返回 404。
func (s *Service) GetByKey(ctx context.Context, key string) (Board, error) {
	var b Board
	err := s.db.QueryRowContext(ctx,
		`SELECT id, "key", name, description FROM boards WHERE "key" = ?`, key).
		Scan(&b.ID, &b.Key, &b.Name, &b.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return Board{}, apperr.NotFound("看板不存在：" + key)
	}
	if err != nil {
		return Board{}, apperr.Internal(err)
	}
	return b, nil
}

// GetByID 按 ID 查看板，阶段配置接口用它反查所属看板。
func (s *Service) GetByID(ctx context.Context, id int64) (Board, error) {
	var b Board
	err := s.db.QueryRowContext(ctx,
		`SELECT id, "key", name, description FROM boards WHERE id = ?`, id).
		Scan(&b.ID, &b.Key, &b.Name, &b.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return Board{}, apperr.NotFound("看板不存在")
	}
	if err != nil {
		return Board{}, apperr.Internal(err)
	}
	return b, nil
}

// Exists 判断看板是否存在，供 application 模块校验。
func (s *Service) Exists(ctx context.Context, id int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM boards WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, apperr.Internal(err)
	}
	return true, nil
}

// SummaryOf 统计看板的推进情况。分类按阶段的 kind 决定，
// 所以用户新增/改名阶段之后统计口径依然正确。
func (s *Service) SummaryOf(ctx context.Context, boardID int64) (Summary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT st.kind, COUNT(*)
		FROM applications a
		JOIN stages st ON st.board_id = a.board_id AND st."key" = a.stage_key
		WHERE a.board_id = ?
		GROUP BY st.kind`, boardID)
	if err != nil {
		return Summary{}, apperr.Internal(err)
	}
	defer rows.Close()

	var sum Summary
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			return Summary{}, apperr.Internal(err)
		}
		switch workflow.Kind(kind) {
		case workflow.KindOffer:
			sum.Offer = count
		case workflow.KindRejected:
			sum.Rejected = count
		default:
			sum.Active += count
		}
		sum.Total += count
	}
	if err := rows.Err(); err != nil {
		return Summary{}, apperr.Internal(err)
	}

	now := time.Now().UTC()
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM interview_rounds r
		JOIN applications a ON a.id = r.application_id
		WHERE a.board_id = ? AND r.result = 'pending'
		  AND r.scheduled_at IS NOT NULL AND r.scheduled_at BETWEEN ? AND ?`,
		boardID, now.Format(time.RFC3339), now.AddDate(0, 0, 7).Format(time.RFC3339),
	).Scan(&sum.Upcoming); err != nil {
		return Summary{}, apperr.Internal(err)
	}

	if sum.Total > 0 {
		sum.Percent = (sum.Offer + sum.Rejected) * 100 / sum.Total
	}
	return sum, nil
}
