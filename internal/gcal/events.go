package gcal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Event 是一条日历事件，只保留界面上要用的字段。
type Event struct {
	ID          string    `json:"id"`
	Summary     string    `json:"summary"`
	Description string    `json:"description"`
	Location    string    `json:"location"`
	HTMLLink    string    `json:"html_link"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"all_day"`
}

// EventInput 是要写进日历的内容。
type EventInput struct {
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
}

// apiEvent 是 Calendar v3 的事件结构。
// start/end 是二选一：定时事件给 dateTime，全天事件给 date。
type apiEvent struct {
	ID          string   `json:"id,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Location    string   `json:"location,omitempty"`
	HTMLLink    string   `json:"htmlLink,omitempty"`
	Status      string   `json:"status,omitempty"`
	Start       *apiTime `json:"start,omitempty"`
	End         *apiTime `json:"end,omitempty"`
}

type apiTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

func (t *apiTime) parse() (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	if t.DateTime != "" {
		parsed, err := time.Parse(time.RFC3339, t.DateTime)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, false
	}
	if t.Date != "" {
		parsed, err := time.ParseInLocation("2006-01-02", t.Date, time.Local)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, true
	}
	return time.Time{}, false
}

func (e apiEvent) toEvent() Event {
	start, allDay := e.Start.parse()
	end, _ := e.End.parse()
	return Event{
		ID:          e.ID,
		Summary:     e.Summary,
		Description: e.Description,
		Location:    e.Location,
		HTMLLink:    e.HTMLLink,
		Start:       start,
		End:         end,
		AllDay:      allDay,
	}
}

func toAPITime(t time.Time) *apiTime {
	return &apiTime{DateTime: t.Format(time.RFC3339)}
}

// ListEvents 拉一段时间窗内的日程。
// singleEvents=true 把重复事件展开成一条条实例，orderBy=startTime 才允许用。
func (c *Client) ListEvents(ctx context.Context, accessToken, calendarID string, from, to time.Time) ([]Event, error) {
	q := url.Values{
		"timeMin":      {from.Format(time.RFC3339)},
		"timeMax":      {to.Format(time.RFC3339)},
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
		"maxResults":   {"250"},
	}
	endpoint := fmt.Sprintf("%s/calendars/%s/events?%s", APIBase, url.PathEscape(calendarID), q.Encode())

	var out struct {
		Items []apiEvent `json:"items"`
	}
	if err := c.call(ctx, accessToken, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}

	events := make([]Event, 0, len(out.Items))
	for _, item := range out.Items {
		if item.Status == "cancelled" {
			continue
		}
		ev := item.toEvent()
		if ev.Start.IsZero() {
			continue
		}
		events = append(events, ev)
	}
	return events, nil
}

// CreateEvent 建一条日程，返回它的 event id。
func (c *Client) CreateEvent(ctx context.Context, accessToken, calendarID string, in EventInput) (string, error) {
	endpoint := fmt.Sprintf("%s/calendars/%s/events", APIBase, url.PathEscape(calendarID))
	var out apiEvent
	if err := c.call(ctx, accessToken, http.MethodPost, endpoint, toAPIEvent(in), &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// UpdateEvent 改一条已有日程。
func (c *Client) UpdateEvent(ctx context.Context, accessToken, calendarID, eventID string, in EventInput) error {
	endpoint := fmt.Sprintf("%s/calendars/%s/events/%s",
		APIBase, url.PathEscape(calendarID), url.PathEscape(eventID))
	return c.call(ctx, accessToken, http.MethodPut, endpoint, toAPIEvent(in), nil)
}

// DeleteEvent 删一条日程。事件已经不在了（404/410）视为成功——
// 用户可能已经在 Google 那边手动删掉，没必要因此让看板的操作失败。
func (c *Client) DeleteEvent(ctx context.Context, accessToken, calendarID, eventID string) error {
	endpoint := fmt.Sprintf("%s/calendars/%s/events/%s",
		APIBase, url.PathEscape(calendarID), url.PathEscape(eventID))
	err := c.call(ctx, accessToken, http.MethodDelete, endpoint, nil, nil)
	var apiErr *APIError
	if AsAPIError(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusGone) {
		return nil
	}
	return err
}

func toAPIEvent(in EventInput) apiEvent {
	return apiEvent{
		Summary:     in.Summary,
		Description: in.Description,
		Location:    in.Location,
		Start:       toAPITime(in.Start),
		End:         toAPITime(in.End),
	}
}

// APIError 是 Google 返回的错误，带上状态码供调用方分流（401 去刷新，404 当作已删）。
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Google Calendar 返回 HTTP %d: %s", e.Status, e.Message)
}

// AsAPIError 是 errors.As 的窄封装，省得调用方每次都 import errors。
func AsAPIError(err error, target **APIError) bool {
	if err == nil {
		return false
	}
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}

// call 发一次带 Bearer 的请求。body 为 nil 表示不带请求体，out 为 nil 表示不关心响应。
func (c *Client) call(ctx context.Context, accessToken, method, endpoint string, body, out any) error {
	if !c.Available() {
		return ErrNotConfigured
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("序列化请求体失败: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Google Calendar 失败: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("读取 Google Calendar 响应失败: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &APIError{Status: res.StatusCode, Message: errMessage(raw)}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析 Google Calendar 响应失败: %w", err)
	}
	return nil
}

// errMessage 从 Google 的错误信封里抠出人话，抠不出就退回原文的前一段。
func errMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
