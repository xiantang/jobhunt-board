package calendar

import (
	"context"
	"database/sql"
	"time"

	"interview/internal/application"
	"interview/internal/gcal"
	"interview/internal/platform/apperr"
)

// DefaultWindowDays 是日程页默认的时间窗：一周。
const DefaultWindowDays = 7

// StartOfWeek 返回 t 所在那一周的周日零点。
//
// 日程页固定按自然周显示：一周七天、周日打头。从「今天」起算七天也能凑够
// 一周，但那样每天的列都在漂移，翻页之后「周三在第几列」全靠数——
// 对齐到自然周，位置才是稳定的锚点。
func StartOfWeek(t time.Time) time.Time {
	local := t.Local()
	y, m, d := local.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	return day.AddDate(0, 0, -int(day.Weekday()))
}

// RoundStore 是本模块对面试轮次的最小依赖。
type RoundStore interface {
	// UpcomingRounds 返回时间窗内已排期的面试（跨看板）。
	UpcomingRounds(ctx context.Context, from, to time.Time) ([]application.ScheduledRound, error)
	// UnscheduledRounds 返回还没定时间的面试，日程页单独列出来提醒。
	UnscheduledRounds(ctx context.Context) ([]application.ScheduledRound, error)
	// RoundForSync 返回一轮面试和它所属的流程，用来拼日程标题。
	RoundForSync(ctx context.Context, roundID int64) (application.ScheduledRound, error)
	// SetGoogleEventID 记下这一轮对应的 Google 事件。
	SetGoogleEventID(ctx context.Context, roundID int64, eventID string) error
}

// Service 是日历集成的业务入口。
type Service struct {
	client *gcal.Client
	store  *store
	rounds RoundStore
}

// NewService 构造日历服务。client 不可用（没配 OAuth）时，
// Enabled() 为 false，所有写操作都会被挡在门口。
func NewService(db *sql.DB, client *gcal.Client, rounds RoundStore) *Service {
	return &Service{client: client, store: &store{db: db}, rounds: rounds}
}

// Enabled 表示配了 OAuth 客户端，页面上才显示「连接 Google 日历」。
func (s *Service) Enabled() bool { return s.client != nil && s.client.Available() }

// RedirectURL 返回回调地址，配置没配对时页面上要提示用户。
func (s *Service) RedirectURL() string {
	if s.client == nil {
		return ""
	}
	return s.client.RedirectURL()
}

// Status 返回当前连接状态。
type Status struct {
	Enabled     bool     `json:"enabled"`   // 配了 OAuth 客户端
	Connected   bool     `json:"connected"` // 已经授权过
	Account     *Account `json:"account"`
	RedirectURL string   `json:"redirect_url"`
}

// Status 查连接状态，页面据此决定显示「连接」还是「已连接到 xxx」。
func (s *Service) Status(ctx context.Context) (Status, error) {
	out := Status{Enabled: s.Enabled(), RedirectURL: s.RedirectURL()}
	if !out.Enabled {
		return out, nil
	}
	r, err := s.store.load(ctx)
	if isNoRows(err) {
		return out, nil
	}
	if err != nil {
		return Status{}, apperr.Internal(err)
	}
	account := r.Account
	out.Connected, out.Account = true, &account
	return out, nil
}

// Connected 表示已经授权过。用来决定「要不要自动同步」——
// 没连接就安静跳过，而不是每次保存都弹一句「还没连接 Google 日历」。
func (s *Service) Connected(ctx context.Context) bool {
	if !s.Enabled() {
		return false
	}
	_, err := s.store.load(ctx)
	return err == nil
}

// AuthURL 拼授权地址。
func (s *Service) AuthURL(state string) (string, error) {
	if !s.Enabled() {
		return "", apperr.Unavailable(apperr.CodeGoogleNotConnected,
			"没有配置 GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET，无法连接 Google 日历")
	}
	return s.client.AuthURL(state), nil
}

// Connect 用回调拿到的授权码换 token 并存下来。
func (s *Service) Connect(ctx context.Context, code string) (Account, error) {
	if !s.Enabled() {
		return Account{}, apperr.Unavailable(apperr.CodeGoogleNotConnected, "没有配置 Google OAuth 客户端")
	}
	token, err := s.client.Exchange(ctx, code)
	if err != nil {
		return Account{}, apperr.Conflict(apperr.CodeGoogleNotConnected, "Google 授权失败："+err.Error())
	}
	if token.RefreshToken == "" {
		// 没拿到 refresh token 就等于只连了一小时，重启即断，不如当场报错。
		return Account{}, apperr.Conflict(apperr.CodeGoogleNotConnected,
			"Google 没有返回 refresh token，请到账号的第三方授权里移除本应用后重新连接")
	}

	email, err := s.client.Email(ctx, token.AccessToken)
	if err != nil {
		email = "" // 拿不到邮箱不影响用，页面上少显示一行而已
	}

	r := record{
		Account: Account{Email: email, CalendarID: "primary", ConnectedAt: time.Now()},
		token:   token,
	}
	if err := s.store.save(ctx, r); err != nil {
		return Account{}, err
	}
	return r.Account, nil
}

// Disconnect 断开连接并清掉已同步的事件 id。
func (s *Service) Disconnect(ctx context.Context) error { return s.store.clear(ctx) }

// accessToken 取一个可用的 access token，过期就用 refresh token 换新的并落库。
func (s *Service) accessToken(ctx context.Context) (string, string, error) {
	r, err := s.store.load(ctx)
	if isNoRows(err) {
		return "", "", notConnected()
	}
	if err != nil {
		return "", "", apperr.Internal(err)
	}
	if r.token.Valid() {
		return r.token.AccessToken, r.CalendarID, nil
	}

	refreshed, err := s.client.Refresh(ctx, r.token.RefreshToken)
	if err != nil {
		return "", "", apperr.Conflict(apperr.CodeGoogleNotConnected,
			"Google 授权已失效，请重新连接（"+err.Error()+"）")
	}
	if err := s.store.saveToken(ctx, refreshed); err != nil {
		return "", "", err
	}
	return refreshed.AccessToken, r.CalendarID, nil
}
