package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"interview/internal/calendar"
	"interview/internal/gcal"
)

// thisWeekAt 返回本周第 weekday 天（0 = 周日）的 hour 点整。
//
// 日程页按自然周显示，测试数据必须落在当前这一周里：用「明天」的话，
// 今天是周六时明天就已经是下一周，那条记录会掉出时间窗，
// 测试跟着周几随机红——这种失败最难查。
func thisWeekAt(weekday, hour int) time.Time {
	return calendar.StartOfWeek(time.Now()).
		AddDate(0, 0, weekday).
		Add(time.Duration(hour) * time.Hour)
}

// googleStub 是一个假的 Google 端点：token / userinfo / events 都在这里应付掉，
// 不打真网络，也不需要真凭证。
type googleStub struct {
	server *httptest.Server
	// created 记下建过的事件，断言标题和时间用。
	created  []map[string]any
	updated  []string
	deleted  []string
	listBack []map[string]any
	failNext bool
}

func newGoogleStub(t *testing.T) *googleStub {
	t.Helper()
	s := &googleStub{}
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"email": "me@example.com"})
	})
	mux.HandleFunc("/calendars/", func(w http.ResponseWriter, r *http.Request) {
		if s.failNext {
			s.failNext = false
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "日历炸了"}})
			return
		}
		switch {
		case r.Method == http.MethodGet:
			writeJSON(w, map[string]any{"items": s.listBack})
		case r.Method == http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			s.created = append(s.created, body)
			writeJSON(w, map[string]any{"id": fmt.Sprintf("ev-%d", len(s.created))})
		case r.Method == http.MethodPut:
			s.updated = append(s.updated, lastSegment(r.URL.Path))
			writeJSON(w, map[string]any{"id": lastSegment(r.URL.Path)})
		case r.Method == http.MethodDelete:
			s.deleted = append(s.deleted, lastSegment(r.URL.Path))
			w.WriteHeader(http.StatusNoContent)
		}
	})

	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)

	// 把包级端点指向 stub，测完还原。
	oldToken, oldAPI, oldUser := gcal.TokenEndpoint, gcal.APIBase, gcal.UserInfoURL
	gcal.TokenEndpoint = s.server.URL + "/token"
	gcal.APIBase = s.server.URL
	gcal.UserInfoURL = s.server.URL + "/userinfo"
	t.Cleanup(func() {
		gcal.TokenEndpoint, gcal.APIBase, gcal.UserInfoURL = oldToken, oldAPI, oldUser
	})
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func lastSegment(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return parts[len(parts)-1]
}

// setupGoogle 起一个连好 Google 的引擎。
func setupGoogle(t *testing.T) (*gin.Engine, *googleStub) {
	t.Helper()
	stub := newGoogleStub(t)
	r := setupWith(t, nil, gcal.New(gcal.Config{ClientID: "cid", ClientSecret: "secret"}))
	connectGoogle(t, r)
	return r, stub
}

// connectGoogle 跑一遍 OAuth 回调，把凭证写进库。
func connectGoogle(t *testing.T, r *gin.Engine) {
	t.Helper()

	// 先走 connect 拿 state cookie。
	req := httptest.NewRequest(http.MethodGet, "/oauth/google/connect", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("connect 应当 302，实际 %d", w.Code)
	}
	var state string
	for _, c := range w.Result().Cookies() {
		if c.Name == "google_oauth_state" {
			state = c.Value
		}
	}
	if state == "" {
		t.Fatal("没有下发 state cookie")
	}

	req = httptest.NewRequest(http.MethodGet, "/oauth/google/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: "google_oauth_state", Value: state})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "google=connected") {
		t.Fatalf("回调应当跳回日程页并带 connected，实际 %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestGoogleConnectAndStatus(t *testing.T) {
	r, _ := setupGoogle(t)

	code, body := do(t, r, http.MethodGet, "/api/google/status", nil)
	if code != http.StatusOK {
		t.Fatalf("状态返回 %d：%v", code, body)
	}
	status := body["status"].(map[string]any)
	if status["enabled"] != true || status["connected"] != true {
		t.Fatalf("应当是已启用且已连接：%v", status)
	}
	if got := status["account"].(map[string]any)["email"]; got != "me@example.com" {
		t.Fatalf("账号 = %v", got)
	}

	// 断开后状态回落，且不影响其他功能。
	if code, body = do(t, r, http.MethodPost, "/api/google/disconnect", map[string]any{}); code != http.StatusOK {
		t.Fatalf("断开返回 %d：%v", code, body)
	}
	_, body = do(t, r, http.MethodGet, "/api/google/status", nil)
	if body["status"].(map[string]any)["connected"] != false {
		t.Fatalf("断开后应当是未连接：%v", body["status"])
	}
}

// 排期自动推到日历，改期走 update，删除撤掉日程。
func TestRoundSyncsToGoogleCalendar(t *testing.T) {
	r, stub := setupGoogle(t)

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "同步公司", "role": "后端", "owner_id": 1, "stage_key": "round_1"})
	if code != http.StatusCreated {
		t.Fatalf("建卡返回 %d：%v", code, body)
	}
	appID := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))

	at := time.Now().Add(48 * time.Hour).Format(time.RFC3339)
	code, body = do(t, r, http.MethodPost, "/api/applications/"+appID+"/rounds",
		map[string]any{"scheduled_at": at, "duration_min": 45, "interviewer": "张工"})
	if code != http.StatusCreated {
		t.Fatalf("排期返回 %d：%v", code, body)
	}
	if body["calendar_warning"] != nil {
		t.Fatalf("同步不该报错：%v", body["calendar_warning"])
	}
	if len(stub.created) != 1 {
		t.Fatalf("应当建了一条日程，实际 %d 条", len(stub.created))
	}
	if title := stub.created[0]["summary"].(string); !strings.Contains(title, "同步公司") ||
		!strings.Contains(title, "一面") {
		t.Fatalf("日程标题 = %q", title)
	}
	roundID := strconv.Itoa(int(body["round"].(map[string]any)["id"].(float64)))

	// 改期：已经有 event id，应当走 update 而不是再建一条。
	code, body = do(t, r, http.MethodPatch, "/api/rounds/"+roundID,
		map[string]any{"scheduled_at": time.Now().Add(72 * time.Hour).Format(time.RFC3339)})
	if code != http.StatusOK {
		t.Fatalf("改期返回 %d：%v", code, body)
	}
	if len(stub.created) != 1 || len(stub.updated) != 1 {
		t.Fatalf("改期应当走 update：created=%d updated=%v", len(stub.created), stub.updated)
	}

	// 删除轮次：日程一并撤掉。
	if code, body = do(t, r, http.MethodDelete, "/api/rounds/"+roundID, nil); code != http.StatusOK {
		t.Fatalf("删除返回 %d：%v", code, body)
	}
	if len(stub.deleted) != 1 {
		t.Fatalf("应当撤掉日程，实际 deleted=%v", stub.deleted)
	}
}

// 时间被清空 = 退回「待安排」，日程要从日历上撤掉。
func TestClearingScheduleRemovesEvent(t *testing.T) {
	r, stub := setupGoogle(t)

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "撤期公司", "owner_id": 1, "stage_key": "round_1"})
	if code != http.StatusCreated {
		t.Fatalf("建卡返回 %d：%v", code, body)
	}
	appID := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))

	at := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	_, body = do(t, r, http.MethodPost, "/api/applications/"+appID+"/rounds",
		map[string]any{"scheduled_at": at})
	roundID := strconv.Itoa(int(body["round"].(map[string]any)["id"].(float64)))
	if len(stub.created) != 1 {
		t.Fatalf("应当先建了一条日程")
	}

	if code, body = do(t, r, http.MethodPatch, "/api/rounds/"+roundID,
		map[string]any{"scheduled_at": ""}); code != http.StatusOK {
		t.Fatalf("清空时间返回 %d：%v", code, body)
	}
	if len(stub.deleted) != 1 {
		t.Fatalf("退回待安排应当撤掉日程，实际 deleted=%v", stub.deleted)
	}
}

// Google 挂了不能让排期存不下来。
func TestCalendarFailureDoesNotBlockScheduling(t *testing.T) {
	r, stub := setupGoogle(t)

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "容错公司", "owner_id": 1, "stage_key": "round_1"})
	if code != http.StatusCreated {
		t.Fatalf("建卡返回 %d：%v", code, body)
	}
	appID := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))

	stub.failNext = true
	at := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	code, body = do(t, r, http.MethodPost, "/api/applications/"+appID+"/rounds",
		map[string]any{"scheduled_at": at})

	if code != http.StatusCreated {
		t.Fatalf("日历失败不该让排期失败，实际 %d：%v", code, body)
	}
	if body["calendar_warning"] == nil {
		t.Fatalf("应当把同步失败的原因带回来：%v", body)
	}
	if body["round"].(map[string]any)["scheduled_at"] == nil {
		t.Fatalf("排期本身应当已经存下来了：%v", body["round"])
	}
}

// 日程页的核心：看板的面试和 Google 日历的会议铺在同一条时间线上，
// 撞期要标出来。
func TestAgendaMergesBothSources(t *testing.T) {
	r, stub := setupGoogle(t)

	// 本周三 14:00 的面试。
	at := thisWeekAt(3, 14)

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "时间线公司", "owner_id": 1, "stage_key": "round_1"})
	if code != http.StatusCreated {
		t.Fatalf("建卡返回 %d：%v", code, body)
	}
	appID := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))
	if code, body = do(t, r, http.MethodPost, "/api/applications/"+appID+"/rounds",
		map[string]any{"scheduled_at": at.Format(time.RFC3339), "duration_min": 60}); code != http.StatusCreated {
		t.Fatalf("排期返回 %d：%v", code, body)
	}

	// Google 日历上同一时段已经有个周会——这就是撞期。
	stub.listBack = []map[string]any{
		{
			"id": "g1", "summary": "组内周会",
			"start": map[string]any{"dateTime": at.Add(30 * time.Minute).Format(time.RFC3339)},
			"end":   map[string]any{"dateTime": at.Add(90 * time.Minute).Format(time.RFC3339)},
		},
		{
			"id": "g2", "summary": "取消掉的会", "status": "cancelled",
			"start": map[string]any{"dateTime": at.Format(time.RFC3339)},
			"end":   map[string]any{"dateTime": at.Add(time.Hour).Format(time.RFC3339)},
		},
	}

	code, body = do(t, r, http.MethodGet, "/api/agenda", nil)
	if code != http.StatusOK {
		t.Fatalf("日程返回 %d：%v", code, body)
	}
	if body["warning"] != nil && body["warning"] != "" {
		t.Fatalf("不该有警告：%v", body["warning"])
	}

	days := body["days"].([]any)
	if len(days) != 7 { // 默认一周，空的那天也占一格
		t.Fatalf("默认应当是一周七天，实际 %d 天", len(days))
	}

	entries := findDayEntries(t, days, at.Format("2006-01-02"))
	if len(entries) != 2 {
		t.Fatalf("那天应当有两条（面试 + 周会），已取消的不算，实际 %d 条：%v", len(entries), entries)
	}

	var sawInterview, sawGoogle bool
	for _, raw := range entries {
		e := raw.(map[string]any)
		if e["conflict"] != true {
			t.Fatalf("两条时间重叠，都该标撞期：%v", e)
		}
		switch e["source"] {
		case "interview":
			sawInterview = true
			if e["synced"] != true {
				t.Fatalf("排期时已经自动同步过了：%v", e)
			}
			if !strings.Contains(e["title"].(string), "时间线公司") {
				t.Fatalf("面试标题 = %v", e["title"])
			}
		case "google":
			sawGoogle = true
			if e["title"] != "组内周会" {
				t.Fatalf("日历标题 = %v", e["title"])
			}
		}
	}
	if !sawInterview || !sawGoogle {
		t.Fatalf("两种来源都该在：interview=%v google=%v", sawInterview, sawGoogle)
	}
}

// Google 拉不回来时，看板那半边照常显示，只挂一个 warning。
func TestAgendaSurvivesGoogleFailure(t *testing.T) {
	r, stub := setupGoogle(t)

	at := thisWeekAt(3, 10)

	_, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "降级公司", "owner_id": 1, "stage_key": "round_1"})
	appID := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))
	do(t, r, http.MethodPost, "/api/applications/"+appID+"/rounds",
		map[string]any{"scheduled_at": at.Format(time.RFC3339)})

	stub.failNext = true
	code, body := do(t, r, http.MethodGet, "/api/agenda", nil)
	if code != http.StatusOK {
		t.Fatalf("Google 挂了不该让整页失败，实际 %d：%v", code, body)
	}
	if body["warning"] == nil || body["warning"] == "" {
		t.Fatalf("应当挂一个警告说明日历没拉回来：%v", body)
	}
	entries := findDayEntries(t, body["days"].([]any), at.Format("2006-01-02"))
	if len(entries) != 1 || entries[0].(map[string]any)["source"] != "interview" {
		t.Fatalf("看板的面试照常显示：%v", entries)
	}
}

// 没连接 Google 时，日程页只显示看板这一半，不报错。
func TestAgendaWithoutGoogle(t *testing.T) {
	r := setup(t)

	code, body := do(t, r, http.MethodGet, "/api/agenda", nil)
	if code != http.StatusOK {
		t.Fatalf("没连 Google 也该能看日程，实际 %d：%v", code, body)
	}
	status := body["status"].(map[string]any)
	if status["enabled"] != false || status["connected"] != false {
		t.Fatalf("应当是未启用未连接：%v", status)
	}
	// 种子里有两条已排期的面试，日程页要能看到它们（时间窗内的部分）。
	if body["unscheduled"] == nil {
		t.Fatalf("待安排列表字段应当存在：%v", body)
	}

	// 不传 from 时按自然周返回：七天，周日打头。
	// 从「今天」起算也能凑够七天，但那样每天的列会随今天是周几而漂移。
	days := body["days"].([]any)
	if len(days) != 7 {
		t.Fatalf("默认该返回一整周，实际 %d 天", len(days))
	}
	if first := days[0].(map[string]any); first["weekday"] != "周日" {
		t.Fatalf("第一天该是周日，实际 %v（%v）", first["weekday"], first["date"])
	}
	if last := days[6].(map[string]any); last["weekday"] != "周六" {
		t.Fatalf("最后一天该是周六，实际 %v（%v）", last["weekday"], last["date"])
	}
}

func TestCalendarPageRenders(t *testing.T) {
	r := setup(t)

	req := httptest.NewRequest(http.MethodGet, "/boards/JOBHUNT/calendar", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("日程页返回 %d：%s", w.Code, w.Body.String())
	}
	html := w.Body.String()
	for _, want := range []string{`id="agenda"`, `id="agenda-data"`, "calendar.js", "回看板"} {
		if !strings.Contains(html, want) {
			t.Fatalf("日程页缺少 %q", want)
		}
	}
}

// findDayEntries 取某一天的条目。
func findDayEntries(t *testing.T, days []any, date string) []any {
	t.Helper()
	for _, raw := range days {
		day := raw.(map[string]any)
		if day["date"] == date {
			return day["entries"].([]any)
		}
	}
	t.Fatalf("日程里没有 %s 这一天", date)
	return nil
}

// 卡片已经走到终态时，它下面挂着的待安排轮次不会再发生了，
// 不该继续出现在「还没定时间」里催人去约。
func TestAgendaHidesUnscheduledOnEndedApplications(t *testing.T) {
	r := setup(t)

	// 一张还在推进的卡：待安排的一面应当显示。
	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "推进中公司", "owner_id": 1, "stage_key": "round_1"})
	if code != http.StatusCreated {
		t.Fatalf("建卡返回 %d：%v", code, body)
	}

	// 另一张：同样有待安排的一面，但流程已经结束。
	code, body = do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "已结束公司", "owner_id": 1, "stage_key": "round_1"})
	if code != http.StatusCreated {
		t.Fatalf("建卡返回 %d：%v", code, body)
	}
	endedID := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))
	if code, body = do(t, r, http.MethodPatch, "/api/applications/"+endedID+"/stage",
		map[string]any{"to": "rejected"}); code != http.StatusOK {
		t.Fatalf("流转到已结束返回 %d：%v", code, body)
	}

	// 拿到 Offer 也是终态，同样不该再催。
	code, body = do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "拿到 Offer 公司", "owner_id": 1, "stage_key": "round_1"})
	if code != http.StatusCreated {
		t.Fatalf("建卡返回 %d：%v", code, body)
	}
	offerID := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))
	if code, body = do(t, r, http.MethodPatch, "/api/applications/"+offerID+"/stage",
		map[string]any{"to": "offer"}); code != http.StatusOK {
		t.Fatalf("流转到已发 Offer 返回 %d：%v", code, body)
	}

	_, body = do(t, r, http.MethodGet, "/api/agenda", nil)
	titles := make([]string, 0, 4)
	for _, raw := range body["unscheduled"].([]any) {
		titles = append(titles, raw.(map[string]any)["title"].(string))
	}
	joined := strings.Join(titles, " / ")

	if !strings.Contains(joined, "推进中公司") {
		t.Fatalf("还在推进的卡片该继续提醒：%s", joined)
	}
	for _, gone := range []string{"已结束公司", "拿到 Offer 公司"} {
		if strings.Contains(joined, gone) {
			t.Fatalf("走到终态的卡片不该再出现在「还没定时间」里：%s", joined)
		}
	}
}
