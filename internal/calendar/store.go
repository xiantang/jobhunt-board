// Package calendar 是 Google Calendar 的业务层：保管授权凭证、
// 把面试推成日程、把日历上已有的会议拉回来和看板拼成一条时间线。
//
// 传输细节在 internal/gcal，这里只关心「什么时候同步、同步成什么样」。
package calendar

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"interview/internal/gcal"
	"interview/internal/platform/apperr"
)

// Account 是已连接的 Google 账号。
type Account struct {
	Email       string    `json:"email"`
	CalendarID  string    `json:"calendar_id"`
	ConnectedAt time.Time `json:"connected_at"`
}

// store 读写 google_accounts 表。单用户，永远只有 id = 1 这一行。
type store struct{ db *sql.DB }

type record struct {
	Account
	token gcal.Token
}

// load 读出唯一那行凭证，没连接过时返回 sql.ErrNoRows。
func (s *store) load(ctx context.Context) (record, error) {
	var (
		r                     record
		expiry, connectedAt   string
		access, refresh, mail string
		calendarID            string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT email, calendar_id, access_token, refresh_token, expiry, connected_at
		FROM google_accounts WHERE id = 1`).
		Scan(&mail, &calendarID, &access, &refresh, &expiry, &connectedAt)
	if err != nil {
		return record{}, err
	}

	r.Email, r.CalendarID = mail, calendarID
	r.ConnectedAt, _ = time.Parse(time.RFC3339, connectedAt)
	r.token = gcal.Token{AccessToken: access, RefreshToken: refresh}
	r.token.Expiry, _ = time.Parse(time.RFC3339, expiry)
	return r, nil
}

// save 写入或覆盖凭证。
//
// 不用 upsert：SQLite 写 ON CONFLICT，MySQL 写 ON DUPLICATE KEY UPDATE，
// 两边语法对不上。这张表永远只有 id = 1 一行，先更新、没更新到再插入，
// 是两边都认的写法。单用户，不存在两个请求同时来抢这一行。
func (s *store) save(ctx context.Context, r record) error {
	expiry := r.token.Expiry.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE google_accounts
		SET email = ?, calendar_id = ?, access_token = ?, refresh_token = ?, expiry = ?
		WHERE id = 1`,
		r.Email, r.CalendarID, r.token.AccessToken, r.token.RefreshToken, expiry)
	if err != nil {
		return apperr.Internal(err)
	}
	// connected_at 只在第一次连接时写：更新分支不碰它，保留最早那次的时间。
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO google_accounts (id, email, calendar_id, access_token, refresh_token, expiry, connected_at)
		VALUES (1, ?, ?, ?, ?, ?, ?)`,
		r.Email, r.CalendarID, r.token.AccessToken, r.token.RefreshToken, expiry,
		r.ConnectedAt.UTC().Format(time.RFC3339)); err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// saveToken 只更新刷新后的 access token，不动其余字段。
func (s *store) saveToken(ctx context.Context, t gcal.Token) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE google_accounts SET access_token = ?, refresh_token = ?, expiry = ? WHERE id = 1`,
		t.AccessToken, t.RefreshToken, t.Expiry.UTC().Format(time.RFC3339))
	if err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// clear 断开连接。
func (s *store) clear(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM google_accounts WHERE id = 1`); err != nil {
		return apperr.Internal(err)
	}
	// 事件 id 一并清掉：断开后再连可能换了账号，旧 id 指向的事件已经够不着了。
	if _, err := s.db.ExecContext(ctx, `UPDATE interview_rounds SET google_event_id = ''`); err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// notConnected 是「还没授权」的领域错误，映射成 409 而不是 500。
func notConnected() error {
	return apperr.Conflict(apperr.CodeGoogleNotConnected, "还没连接 Google 日历，请先在日程页面点「连接 Google 日历」")
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
