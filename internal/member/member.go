// Package member 是成员模块：提供可被指定为跟进人的团队成员信息。
// 本轮不做登录，「当前用户」通过下拉切换 + cookie 表达。
package member

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"interview/internal/platform/apperr"
)

// Member 是团队成员。
type Member struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`   // member | lead
	Color  string `json:"color"`  // 头像底色
	Active bool   `json:"active"` // 预留：离职成员不再出现在分配列表
}

// RoleLabel 返回角色中文名，模板里直接调用。
func (m Member) RoleLabel() string {
	if m.Role == RoleLead {
		return "负责人"
	}
	return "团队成员"
}

// Initial 返回头像上的首字母。
func (m Member) Initial() string {
	if m.Name == "" {
		return "?"
	}
	return string([]rune(m.Name)[:1])
}

// 角色取值。
const (
	RoleMember = "member"
	RoleLead   = "lead"
)

// 新成员头像色轮，按创建顺序循环取用。
var palette = []string{"#2563eb", "#16a34a", "#d97706", "#db2777", "#7c3aed", "#0891b2"}

// Service 承载成员相关的业务逻辑与持久化。
type Service struct {
	db *sql.DB
}

// NewService 构造成员服务。
func NewService(db *sql.DB) *Service { return &Service{db: db} }

// List 返回全部在岗成员，按创建顺序。
func (s *Service) List(ctx context.Context) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, role, color, active FROM members WHERE active = 1 ORDER BY id`)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()

	members := make([]Member, 0, 8)
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Name, &m.Role, &m.Color, &m.Active); err != nil {
			return nil, apperr.Internal(err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	return members, nil
}

// Get 按 ID 查成员，不存在返回 404。
func (s *Service) Get(ctx context.Context, id int64) (Member, error) {
	var m Member
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, role, color, active FROM members WHERE id = ?`, id).
		Scan(&m.ID, &m.Name, &m.Role, &m.Color, &m.Active)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, apperr.NotFound("成员不存在")
	}
	if err != nil {
		return Member{}, apperr.Internal(err)
	}
	return m, nil
}

// Exists 判断成员是否存在且在岗，供 application 模块校验跟进人。
func (s *Service) Exists(ctx context.Context, id int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM members WHERE id = ? AND active = 1`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, apperr.Internal(err)
	}
	return true, nil
}

// Create 新增成员，名字唯一。
func (s *Service) Create(ctx context.Context, name, role string) (Member, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Member{}, apperr.Invalid("name", "成员姓名不能为空")
	}
	if len([]rune(name)) > 20 {
		return Member{}, apperr.Invalid("name", "成员姓名不能超过 20 个字符")
	}
	if role == "" {
		role = RoleMember
	}
	if role != RoleMember && role != RoleLead {
		return Member{}, apperr.Invalid("role", "角色只能是 member 或 lead")
	}

	var taken bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM members WHERE name = ?)`, name).Scan(&taken); err != nil {
		return Member{}, apperr.Internal(err)
	}
	if taken {
		return Member{}, apperr.Conflict(apperr.CodeConflict, "已存在同名成员："+name)
	}

	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM members`).Scan(&count); err != nil {
		return Member{}, apperr.Internal(err)
	}
	color := palette[count%int64(len(palette))]

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO members (name, role, color, created_at) VALUES (?, ?, ?, ?)`,
		name, role, color, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return Member{}, apperr.Internal(err)
	}
	id, _ := res.LastInsertId()
	return Member{ID: id, Name: name, Role: role, Color: color, Active: true}, nil
}

// ResolveActor 校验 cookie 里的成员 ID；无效时退回第一个成员，保证操作日志总有署名。
// 实现 ginx.ActorResolver。
func (s *Service) ResolveActor(ctx context.Context, id int64) int64 {
	if id > 0 {
		if ok, err := s.Exists(ctx, id); err == nil && ok {
			return id
		}
	}
	var fallback int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM members WHERE active = 1 ORDER BY id LIMIT 1`).Scan(&fallback); err != nil {
		return 0
	}
	return fallback
}
