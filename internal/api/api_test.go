package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"interview/internal/ai"
	"interview/internal/gcal"
	"interview/internal/platform/ginx"
	"interview/internal/platform/sqlitedb"
	"interview/internal/server"
)

// setup 起一个真实的 gin 引擎 + 临时库（含种子看板与十个默认阶段）。
// 不注入模型，AI 录入处于关闭状态。
func setup(t *testing.T) *gin.Engine { return setupWithModel(t, nil) }

// setupWithModel 同上，但注入一个假模型，用来测 AI 录入而不打真网络。
func setupWithModel(t *testing.T, model ai.Completer) *gin.Engine {
	return setupWith(t, model, gcal.New(gcal.Config{}))
}

// setupWith 起引擎并注入指定的模型与 Google 客户端。
func setupWith(t *testing.T, model ai.Completer, google *gcal.Client) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := sqlitedb.Seed(db); err != nil {
		t.Fatalf("写入种子数据失败: %v", err)
	}
	return server.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), model, google)
}

// do 发一个请求并返回状态码与解析后的 JSON。
func do(t *testing.T, r *gin.Engine, method, path string, body any) (int, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := map[string]any{}
	if w.Body.Len() > 0 && strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析响应失败: %v（body=%s）", err, w.Body.String())
		}
	}
	return w.Code, out
}

func errCode(t *testing.T, body map[string]any) string {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应里没有 error 字段：%v", body)
	}
	return e["code"].(string)
}

// columns 把看板响应拍平成「阶段名 → 卡片数」。
func columns(t *testing.T, board any) map[string]int {
	t.Helper()
	view, ok := board.(map[string]any)
	if !ok {
		t.Fatalf("board 字段类型不对：%T", board)
	}
	out := map[string]int{}
	for _, c := range view["columns"].([]any) {
		col := c.(map[string]any)
		out[col["label"].(string)] = int(col["count"].(float64))
	}
	return out
}

func TestFullFlow(t *testing.T) {
	r := setup(t)

	// 新建投递，落在首列。
	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "某大厂", "role": "后端研发", "channel": "内推", "intent": "high", "owner_id": 1})
	if code != http.StatusCreated {
		t.Fatalf("创建返回 %d：%v", code, body)
	}
	app := body["application"].(map[string]any)
	if app["stage_key"] != "hr_screen" {
		t.Fatalf("新流程阶段 = %v，期望 hr_screen", app["stage_key"])
	}
	id := int(app["id"].(float64))

	// 逐格推进到一面。
	for _, to := range []string{"online_test", "round_1"} {
		code, body = do(t, r, http.MethodPatch, "/api/applications/"+strconv.Itoa(id)+"/stage", map[string]any{"to": to})
		if code != http.StatusOK {
			t.Fatalf("推进到 %s 返回 %d：%v", to, code, body)
		}
	}
	app = body["application"].(map[string]any)
	if app["round_count"].(float64) != 1 {
		t.Fatalf("进入面试阶段应自动建一条待安排记录，实际 %v", app["round_count"])
	}

	// 补上面试时间与会议链接。
	_, detail := do(t, r, http.MethodGet, "/api/applications/"+strconv.Itoa(id), nil)
	roundID := int(detail["rounds"].([]any)[0].(map[string]any)["id"].(float64))
	code, body = do(t, r, http.MethodPatch, "/api/rounds/"+strconv.Itoa(roundID), map[string]any{
		"scheduled_at":  "2030-01-02T14:00:00+08:00",
		"meeting_url":   "https://meeting.example.com/abc",
		"interviewer":   "张工",
		"meeting_place": "",
	})
	if code != http.StatusOK {
		t.Fatalf("排期返回 %d：%v", code, body)
	}
	next := body["application"].(map[string]any)["next_round"].(map[string]any)
	if next["meeting_url"] != "https://meeting.example.com/abc" {
		t.Fatalf("卡片没带出会议链接：%v", next)
	}

	// 任意阶段可直达终态。
	code, body = do(t, r, http.MethodPatch, "/api/applications/"+strconv.Itoa(id)+"/stage", map[string]any{"to": "offer"})
	if code != http.StatusOK {
		t.Fatalf("直达 Offer 返回 %d：%v", code, body)
	}
	if got := columns(t, body["board"])["已发 Offer"]; got != 1 {
		t.Fatalf("Offer 列卡片数 = %d，期望 1", got)
	}

	// 删除这条流程：卡片从看板上消失，面试记录与日志随之清掉。
	code, body = do(t, r, http.MethodDelete, "/api/applications/"+strconv.Itoa(id), nil)
	if code != http.StatusOK {
		t.Fatalf("删除返回 %d：%v", code, body)
	}
	if got := int(body["deleted"].(float64)); got != id {
		t.Fatalf("deleted = %d，期望 %d", got, id)
	}
	if got := columns(t, body["board"])["已发 Offer"]; got != 0 {
		t.Fatalf("删除后 Offer 列卡片数 = %d，期望 0", got)
	}
	if code, _ = do(t, r, http.MethodGet, "/api/applications/"+strconv.Itoa(id), nil); code != http.StatusNotFound {
		t.Fatalf("删除后再查状态码 = %d，期望 404", code)
	}
}

func TestDuplicateApplicationReturns409(t *testing.T) {
	r := setup(t)

	body := map[string]any{"company": "某大厂", "role": "后端研发", "owner_id": 1}
	if code, got := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications", body); code != http.StatusCreated {
		t.Fatalf("首次创建返回 %d：%v", code, got)
	}

	code, got := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications", body)
	if code != http.StatusConflict || errCode(t, got) != "CONFLICT" {
		t.Fatalf("重复投递状态码 = %d，body = %v", code, got)
	}

	// 同公司换个岗位仍然可以建。
	code, got = do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "某大厂", "role": "前端研发", "owner_id": 1})
	if code != http.StatusCreated {
		t.Fatalf("同公司不同岗位返回 %d：%v", code, got)
	}
	id := strconv.Itoa(int(got["application"].(map[string]any)["id"].(float64)))

	// 把它的岗位改成已存在的那个，要被拦住。
	code, got = do(t, r, http.MethodPatch, "/api/applications/"+id, map[string]any{"role": "后端研发"})
	if code != http.StatusConflict || errCode(t, got) != "CONFLICT" {
		t.Fatalf("改岗位撞车状态码 = %d，body = %v", code, got)
	}
}

func TestSkipStageTransition(t *testing.T) {
	r := setup(t)

	// 种子配置里「在线测评」标了可跳过，所以 HR screen 能直达一面。
	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "跳测评公司", "owner_id": 1})
	if code != http.StatusCreated {
		t.Fatalf("创建返回 %d：%v", code, body)
	}
	id := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))

	code, body = do(t, r, http.MethodPatch, "/api/applications/"+id+"/stage", map[string]any{"to": "round_1"})
	if code != http.StatusOK {
		t.Fatalf("跳过在线测评返回 %d：%v", code, body)
	}
	if got := body["application"].(map[string]any)["stage_key"]; got != "round_1" {
		t.Fatalf("阶段 = %v，期望 round_1", got)
	}

	// 把「在线测评」的可跳过关掉，同样的流转就该被拦住。
	_, listed := do(t, r, http.MethodGet, "/api/boards/JOBHUNT/stages", nil)
	var testID string
	for _, x := range listed["stages"].([]any) {
		st := x.(map[string]any)
		if st["key"] == "online_test" {
			if st["skippable"] != true {
				t.Fatalf("种子里「在线测评」应当是可跳过的：%v", st)
			}
			testID = strconv.Itoa(int(st["id"].(float64)))
		}
	}
	if code, body = do(t, r, http.MethodPatch, "/api/stages/"+testID,
		map[string]any{"skippable": false}); code != http.StatusOK {
		t.Fatalf("关闭可跳过返回 %d：%v", code, body)
	}

	code, body = do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "老实排队公司", "owner_id": 1})
	if code != http.StatusCreated {
		t.Fatalf("创建返回 %d：%v", code, body)
	}
	id = strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))

	code, body = do(t, r, http.MethodPatch, "/api/applications/"+id+"/stage", map[string]any{"to": "round_1"})
	if code != http.StatusConflict || errCode(t, body) != "INVALID_TRANSITION" {
		t.Fatalf("关掉可跳过后状态码 = %d，body = %v", code, body)
	}
}

func TestInvalidTransitionReturns409(t *testing.T) {
	r := setup(t)
	// 种子数据里 JOBHUNT-2 停在「在线测评」，直接跳三面属于跨阶段。
	code, body := do(t, r, http.MethodPatch, "/api/applications/2/stage", map[string]any{"to": "round_3"})
	if code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409：%v", code, body)
	}
	if got := errCode(t, body); got != "INVALID_TRANSITION" {
		t.Fatalf("错误码 = %s", got)
	}
}

func TestOwnerRequiredReturns409(t *testing.T) {
	r := setup(t)

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "无人跟进公司"})
	if code != http.StatusCreated {
		t.Fatalf("创建返回 %d：%v", code, body)
	}
	id := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))

	if code, body = do(t, r, http.MethodPatch, "/api/applications/"+id+"/stage",
		map[string]any{"to": "online_test"}); code != http.StatusOK {
		t.Fatalf("推进返回 %d：%v", code, body)
	}
	// 默认配置里「一面」要求先指定跟进人。
	code, body = do(t, r, http.MethodPatch, "/api/applications/"+id+"/stage", map[string]any{"to": "round_1"})
	if code != http.StatusConflict || errCode(t, body) != "OWNER_REQUIRED" {
		t.Fatalf("状态码 = %d，body = %v", code, body)
	}
}

func TestStageConfig(t *testing.T) {
	r := setup(t)

	// 新增阶段：默认插在终态之前。
	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/stages",
		map[string]any{"label": "交叉面", "kind": "interview"})
	if code != http.StatusCreated {
		t.Fatalf("新增阶段返回 %d：%v", code, body)
	}
	created := body["stage"].(map[string]any)
	stageID := strconv.Itoa(int(created["id"].(float64)))
	key := created["key"].(string)

	labels := columnLabels(t, body["board"])
	if labels[len(labels)-3] != "交叉面" {
		t.Fatalf("新阶段应插在终态之前，实际顺序 %v", labels)
	}

	// 改名不改 key，已有卡片不受影响。
	code, body = do(t, r, http.MethodPatch, "/api/stages/"+stageID, map[string]any{"label": "终面"})
	if code != http.StatusOK {
		t.Fatalf("改名返回 %d：%v", code, body)
	}
	if body["stage"].(map[string]any)["key"] != key {
		t.Fatalf("改名后 key 变了：%v", body["stage"])
	}

	// 调序。
	code, body = do(t, r, http.MethodPatch, "/api/stages/"+stageID+"/position", map[string]any{"index": 0})
	if code != http.StatusOK {
		t.Fatalf("调序返回 %d：%v", code, body)
	}
	if got := columnLabels(t, body["board"])[0]; got != "终面" {
		t.Fatalf("调序后首列 = %s", got)
	}

	// 删除空阶段。
	if code, body = do(t, r, http.MethodDelete, "/api/stages/"+stageID, nil); code != http.StatusOK {
		t.Fatalf("删除返回 %d：%v", code, body)
	}
	for _, l := range columnLabels(t, body["board"]) {
		if l == "终面" {
			t.Fatal("删除后阶段仍在看板上")
		}
	}

	// 删除还有卡片的阶段要被拦住。
	_, listed := do(t, r, http.MethodGet, "/api/boards/JOBHUNT/stages", nil)
	var busyID string
	for _, s := range listed["stages"].([]any) {
		st := s.(map[string]any)
		if st["key"] == "round_2" { // 种子数据里「百度」停在二面
			busyID = strconv.Itoa(int(st["id"].(float64)))
		}
	}
	code, body = do(t, r, http.MethodDelete, "/api/stages/"+busyID, nil)
	if code != http.StatusConflict || errCode(t, body) != "CONFLICT" {
		t.Fatalf("状态码 = %d，body = %v", code, body)
	}
}

func TestValidationAndNotFound(t *testing.T) {
	r := setup(t)

	if code, _ := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": ""}); code != http.StatusBadRequest {
		t.Fatalf("空公司名状态码 = %d，期望 400", code)
	}
	if code, _ := do(t, r, http.MethodGet, "/api/applications/9999", nil); code != http.StatusNotFound {
		t.Fatalf("不存在的流程状态码 = %d，期望 404", code)
	}
	if code, _ := do(t, r, http.MethodDelete, "/api/applications/9999", nil); code != http.StatusNotFound {
		t.Fatalf("删除不存在的流程状态码 = %d，期望 404", code)
	}
	if code, _ := do(t, r, http.MethodDelete, "/api/applications/abc", nil); code != http.StatusBadRequest {
		t.Fatalf("非法 ID 状态码 = %d，期望 400", code)
	}
	if code, _ := do(t, r, http.MethodGet, "/api/boards/NOPE/board", nil); code != http.StatusNotFound {
		t.Fatalf("不存在的看板状态码 = %d，期望 404", code)
	}
	if code, _ := do(t, r, http.MethodPatch, "/api/rounds/1",
		map[string]any{"scheduled_at": "明天下午两点"}); code != http.StatusBadRequest {
		t.Fatalf("非法时间格式状态码 = %d，期望 400", code)
	}
}

func TestBoardFilter(t *testing.T) {
	r := setup(t)

	_, body := do(t, r, http.MethodGet, "/api/boards/JOBHUNT/board?stage=round_2", nil)
	counts := columns(t, body)
	if counts["二面"] != 1 {
		t.Fatalf("按阶段筛选后二面 = %d，期望 1", counts["二面"])
	}
	if counts["在线测评"] != 0 {
		t.Fatalf("按阶段筛选后其他列应为空，实际 %d", counts["在线测评"])
	}

	_, body = do(t, r, http.MethodGet, "/api/boards/JOBHUNT/board?upcoming=1", nil)
	total := 0
	for _, n := range columns(t, body) {
		total += n
	}
	if total != 2 { // 种子数据里两条已排期且待进行的面试
		t.Fatalf("只看已排期 = %d 条，期望 2", total)
	}
}

func TestSwitchMemberSetsCookie(t *testing.T) {
	r := setup(t)

	// 种子里只有「我」一个人，先加一个才有得切。
	code, body := do(t, r, http.MethodPost, "/api/members", map[string]any{"name": "同事"})
	if code != http.StatusCreated {
		t.Fatalf("新增成员返回 %d：%v", code, body)
	}
	id := strconv.Itoa(int(body["member"].(map[string]any)["id"].(float64)))

	req := httptest.NewRequest(http.MethodPost, "/api/session/member",
		bytes.NewReader([]byte(`{"member_id":`+id+`}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("切换用户返回 %d：%s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == ginx.CookieCurrentMember && c.Value == id {
			return
		}
	}
	t.Fatalf("没有写入 %s cookie", ginx.CookieCurrentMember)
}

// 种子只写一个成员：这是一个人自己用的看板，默认跟进人就是本人。
func TestSeedHasSingleMember(t *testing.T) {
	r := setup(t)

	code, body := do(t, r, http.MethodGet, "/api/members", nil)
	if code != http.StatusOK {
		t.Fatalf("成员列表返回 %d：%v", code, body)
	}
	members := body["members"].([]any)
	if len(members) != 1 {
		t.Fatalf("种子成员应当只有一个，实际 %d 个：%v", len(members), members)
	}
	if got := members[0].(map[string]any)["name"]; got != "我" {
		t.Fatalf("成员名 = %v，期望「我」", got)
	}
	// 「我是谁」默认落在这个人身上。
	if got := body["current_member_id"]; got != members[0].(map[string]any)["id"] {
		t.Fatalf("当前用户 = %v，期望 %v", got, members[0].(map[string]any)["id"])
	}
}

func TestBoardPageRenders(t *testing.T) {
	r := setup(t)

	req := httptest.NewRequest(http.MethodGet, "/boards/JOBHUNT", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("页面返回 %d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{`data-stage="hr_screen"`, `data-stage="rejected"`, "HRBP 面", `id="board-data"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("首屏缺少 %q", want)
		}
	}
}

// columnLabels 返回看板的列顺序。
func columnLabels(t *testing.T, board any) []string {
	t.Helper()
	view := board.(map[string]any)
	out := make([]string, 0, 12)
	for _, c := range view["columns"].([]any) {
		out = append(out, c.(map[string]any)["label"].(string))
	}
	return out
}
