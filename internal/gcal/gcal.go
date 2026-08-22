// Package gcal 是 Google Calendar 集成的传输层：OAuth2 授权码流程 +
// Calendar v3 的增删改查。和 ai 包一样用标准库直发 HTTP，不引 SDK——
// 这里要用的就是四个接口，一个依赖换不来什么。
//
// 业务规则（哪一轮该同步、事件标题怎么写）在 internal/calendar 里，
// 这个包只负责「把请求发出去，把响应读回来」。
package gcal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Google 的四个端点。抽成变量是为了测试里指向本地 stub。
var (
	AuthEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	TokenEndpoint = "https://oauth2.googleapis.com/token"
	APIBase       = "https://www.googleapis.com/calendar/v3"
	UserInfoURL   = "https://www.googleapis.com/oauth2/v2/userinfo"
)

// Scope 只要事件读写：能建/改/删自己的日程，也能列出日历上已有的会议。
// 比 auth/calendar 窄一档，够用就不多要。
const Scope = "https://www.googleapis.com/auth/calendar.events email"

// ErrNotConfigured 表示没配 OAuth 客户端，调用方应把整个集成降级掉。
var ErrNotConfigured = errors.New("未配置 GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET")

// Config 是 OAuth 客户端配置。
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Timeout      time.Duration
}

// ConfigFromEnv 从环境变量读配置：
// GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET / GOOGLE_REDIRECT_URL。
func ConfigFromEnv() Config {
	return Config{
		ClientID:     strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET")),
		RedirectURL:  strings.TrimSpace(os.Getenv("GOOGLE_REDIRECT_URL")),
	}
}

// Token 是一次授权拿到的凭证。
// RefreshToken 只在第一次授权（prompt=consent）时给，必须落库存下来。
type Token struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

// Valid 表示 access token 还能用（留 60 秒余量，避免边界上打到 401）。
func (t Token) Valid() bool {
	return t.AccessToken != "" && time.Now().Add(time.Minute).Before(t.Expiry)
}

// Client 是 Google 侧的传输客户端。
type Client struct {
	cfg  Config
	http *http.Client
}

// New 构造客户端。没配 client id/secret 时 Available() 为 false。
func New(cfg Config) *Client {
	if cfg.RedirectURL == "" {
		cfg.RedirectURL = "http://localhost:8080/oauth/google/callback"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

// Available 表示配了 OAuth 客户端。
func (c *Client) Available() bool {
	return c != nil && c.cfg.ClientID != "" && c.cfg.ClientSecret != ""
}

// RedirectURL 返回回调地址，页面上提示用户去 Google Cloud 配同一个值。
func (c *Client) RedirectURL() string { return c.cfg.RedirectURL }

// AuthURL 拼出让用户点过去的授权地址。
// access_type=offline + prompt=consent 是拿 refresh token 的必要条件——
// 少了它，授权只给一小时有效的 access token，重启就断。
func (c *Client) AuthURL(state string) string {
	q := url.Values{
		"client_id":     {c.cfg.ClientID},
		"redirect_uri":  {c.cfg.RedirectURL},
		"response_type": {"code"},
		"scope":         {Scope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return AuthEndpoint + "?" + q.Encode()
}

// tokenResponse 是 /token 的响应。
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// Exchange 用授权码换 token。
func (c *Client) Exchange(ctx context.Context, code string) (Token, error) {
	return c.token(ctx, url.Values{
		"code":          {code},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"redirect_uri":  {c.cfg.RedirectURL},
		"grant_type":    {"authorization_code"},
	})
}

// Refresh 用 refresh token 换新的 access token。
// Google 在刷新时不会再给 refresh token，所以这里保留调用方传进来的那个。
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Token, error) {
	tok, err := c.token(ctx, url.Values{
		"refresh_token": {refreshToken},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"grant_type":    {"refresh_token"},
	})
	if err != nil {
		return Token{}, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

func (c *Client) token(ctx context.Context, form url.Values) (Token, error) {
	if !c.Available() {
		return Token{}, ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("构造 token 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := c.http.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("请求 Google token 接口失败: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("读取 token 响应失败: %w", err)
	}
	var parsed tokenResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Token{}, fmt.Errorf("token 响应不是合法 JSON（HTTP %d）: %w", res.StatusCode, err)
	}
	if parsed.Error != "" {
		return Token{}, fmt.Errorf("Google 拒绝了授权: %s %s", parsed.Error, parsed.ErrorDesc)
	}
	if res.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		return Token{}, fmt.Errorf("Google token 接口返回 HTTP %d", res.StatusCode)
	}
	return Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second),
	}, nil
}

// Email 读授权账号的邮箱，只用来在页面上显示「已连接到哪个账号」。
func (c *Client) Email(ctx context.Context, accessToken string) (string, error) {
	var out struct {
		Email string `json:"email"`
	}
	if err := c.call(ctx, accessToken, http.MethodGet, UserInfoURL, nil, &out); err != nil {
		return "", err
	}
	return out.Email, nil
}
