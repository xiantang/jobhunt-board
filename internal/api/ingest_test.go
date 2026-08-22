package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"interview/internal/ai"
)

// foodpandaMail 是一封真实形态的在线测评通知：有公司、有截止日期，
// 但没有具体面试时刻——正好用来验证「截止日期不能被当成排期」。
const foodpandaMail = `Hello Jed,

Thank you for your interest in foodpanda!

As part of the hiring process, please complete the assessment linked below to
showcase your technical skills by solving job-related problems.

Please note that the test link will expires on 28th August 2026 (Friday).

Best Regards,
foodpanda hiring team`

// fakeModel 按预设 JSON 应答，不打真网络。
type fakeModel struct {
	reply string
	err   error
	// 收到的系统提示词，用来断言阶段清单确实喂进去了。
	gotSystem string
}

func (f *fakeModel) Available() bool { return true }

func (f *fakeModel) Complete(_ context.Context, req ai.Request) (json.RawMessage, error) {
	f.gotSystem = req.System
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(f.reply), nil
}

// foodpandaReply 是模型对上面那封邮件的典型输出。
const foodpandaReply = `{
  "company": "foodpanda",
  "role": "",
  "channel": "",
  "stage_key": "online_test",
  "notes": "foodpanda 发来在线测评邀请",
  "summary": "foodpanda 的在线测评通知，需要在 8 月 28 日前完成",
  "confidence": "high",
  "round": {
    "scheduled_at": null,
    "deadline": "2026-08-28",
    "duration_min": 0,
    "mode": "online",
    "meeting_url": "",
    "meeting_place": "",
    "interviewer": "",
    "notes": "在线技术测评，需在链接过期前完成"
  }
}`

func TestIngestParseProducesDraft(t *testing.T) {
	model := &fakeModel{reply: foodpandaReply}
	r := setupWithModel(t, model)

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/ingest/parse",
		map[string]any{"text": foodpandaMail})
	if code != http.StatusOK {
		t.Fatalf("解析返回 %d：%v", code, body)
	}

	// 阶段清单必须随提示词一起喂给模型，否则它会返回本看板没有的 key。
	for _, want := range []string{"online_test", "在线测评", "round_1"} {
		if !strings.Contains(model.gotSystem, want) {
			t.Fatalf("提示词里没带上阶段「%s」：%s", want, model.gotSystem)
		}
	}

	draft := body["draft"].(map[string]any)
	if draft["company"] != "foodpanda" {
		t.Fatalf("公司 = %v，期望 foodpanda", draft["company"])
	}
	if draft["stage_key"] != "online_test" || draft["stage_label"] != "在线测评" {
		t.Fatalf("阶段解析错了：%v / %v", draft["stage_key"], draft["stage_label"])
	}
	if draft["match"] != nil {
		t.Fatalf("看板上本来没有 foodpanda，不该命中已有卡片：%v", draft["match"])
	}

	round := draft["round"].(map[string]any)
	if round["scheduled_at"] != nil {
		t.Fatalf("邮件里没有具体面试时刻，不该排期：%v", round["scheduled_at"])
	}
	if !strings.Contains(round["notes"].(string), "2026-08-28") {
		t.Fatalf("截止日期该折进备注里：%v", round["notes"])
	}
}

func TestIngestConfirmCreatesCardAndRound(t *testing.T) {
	r := setupWithModel(t, &fakeModel{reply: foodpandaReply})

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/ingest/confirm", map[string]any{
		"company":      "foodpanda",
		"role":         "后端工程师",
		"stage_key":    "online_test",
		"create_round": true,
		"round_notes":  "截止：2026-08-28。在线技术测评",
	})
	if code != http.StatusCreated {
		t.Fatalf("确认录入返回 %d：%v", code, body)
	}
	if body["created"] != true {
		t.Fatalf("应当是新建卡片：%v", body["created"])
	}

	app := body["application"].(map[string]any)
	if app["stage_key"] != "online_test" {
		t.Fatalf("卡片应当落在在线测评列：%v", app["stage_key"])
	}
	id := strconv.Itoa(int(app["id"].(float64)))

	_, detail := do(t, r, http.MethodGet, "/api/applications/"+id, nil)
	rounds := detail["rounds"].([]any)
	if len(rounds) != 1 {
		t.Fatalf("应当只录入一轮记录，实际 %d 条", len(rounds))
	}
	got := rounds[0].(map[string]any)
	if got["stage_label"] != "在线测评" {
		t.Fatalf("轮次的阶段快照 = %v", got["stage_label"])
	}
	if !strings.Contains(got["notes"].(string), "2026-08-28") {
		t.Fatalf("轮次备注丢了截止日期：%v", got["notes"])
	}
}

// 流转进面试阶段时系统会自动挂一条「待安排」，AI 录入要补到那一条上，
// 而不是在同一列里再建一条。
func TestIngestConfirmReusesAutoCreatedRound(t *testing.T) {
	r := setupWithModel(t, &fakeModel{reply: foodpandaReply})

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/ingest/confirm", map[string]any{
		"company":      "字节跳动",
		"owner_id":     1, // 「一面」要求先指定跟进人
		"stage_key":    "round_1",
		"create_round": true,
		"scheduled_at": "2026-08-25T14:00:00+08:00",
		"interviewer":  "张三",
	})
	if code != http.StatusCreated {
		t.Fatalf("确认录入返回 %d：%v", code, body)
	}
	id := strconv.Itoa(int(body["application"].(map[string]any)["id"].(float64)))

	_, detail := do(t, r, http.MethodGet, "/api/applications/"+id, nil)
	rounds := detail["rounds"].([]any)
	if len(rounds) != 1 {
		t.Fatalf("同一阶段不该出现两条记录，实际 %d 条：%v", len(rounds), rounds)
	}
	got := rounds[0].(map[string]any)
	if got["interviewer"] != "张三" || got["scheduled_at"] == nil {
		t.Fatalf("信息没有补到自动建的那条上：%v", got)
	}
}

// 已有卡片按公司名命中后复用，不新建第二张。
func TestIngestConfirmMatchesExistingCard(t *testing.T) {
	model := &fakeModel{reply: foodpandaReply}
	r := setupWithModel(t, model)

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "Foodpanda 台湾"})
	if code != http.StatusCreated {
		t.Fatalf("建卡返回 %d：%v", code, body)
	}
	existing := int64(body["application"].(map[string]any)["id"].(float64))

	code, body = do(t, r, http.MethodPost, "/api/boards/JOBHUNT/ingest/parse",
		map[string]any{"text": foodpandaMail})
	if code != http.StatusOK {
		t.Fatalf("解析返回 %d：%v", code, body)
	}
	match, ok := body["draft"].(map[string]any)["match"].(map[string]any)
	if !ok {
		t.Fatalf("「foodpanda」应当命中「Foodpanda 台湾」：%v", body["draft"])
	}
	if int64(match["id"].(float64)) != existing {
		t.Fatalf("命中的卡片不对：%v", match)
	}

	code, body = do(t, r, http.MethodPost, "/api/boards/JOBHUNT/ingest/confirm", map[string]any{
		"application_id": existing,
		"company":        "foodpanda",
		"role":           "后端工程师",
		"stage_key":      "online_test",
		"create_round":   true,
	})
	if code != http.StatusOK {
		t.Fatalf("确认录入返回 %d：%v", code, body)
	}
	if body["created"] != false || body["moved"] != true {
		t.Fatalf("应当是复用已有卡片并流转：%v", body)
	}
	app := body["application"].(map[string]any)
	// 复用时只补空缺，公司名保持原样，不被模型的写法覆盖。
	if app["company"] != "Foodpanda 台湾" {
		t.Fatalf("不该改写已有公司名：%v", app["company"])
	}
	if app["role"] != "后端工程师" {
		t.Fatalf("空着的岗位应当被补上：%v", app["role"])
	}
}

// 没配 key 时 AI 录入整体关闭，返回 503 而不是 500。
func TestIngestDisabledWithoutKey(t *testing.T) {
	r := setup(t)
	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/ingest/parse",
		map[string]any{"text": foodpandaMail})
	if code != http.StatusServiceUnavailable || errCode(t, body) != "AI_UNAVAILABLE" {
		t.Fatalf("状态码 = %d，body = %v", code, body)
	}
}

// 新卡片没有历史，「从一面开始」是个起点而不是流转，
// 不该被「HR screen → 一面」这类相邻性检查挡住。
func TestIngestConfirmStartsAtChosenStage(t *testing.T) {
	r := setupWithModel(t, &fakeModel{reply: foodpandaReply})

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/ingest/confirm", map[string]any{
		"company":      "猎头直推公司",
		"owner_id":     1, // 「一面」要求先指定跟进人
		"stage_key":    "round_1",
		"create_round": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("确认录入返回 %d：%v", code, body)
	}
	app := body["application"].(map[string]any)
	if app["stage_key"] != "round_1" {
		t.Fatalf("卡片应当直接落在一面：%v", app["stage_key"])
	}
	// 直接建在目标列，不经过流转，所以日志里不该有「推进到」。
	if body["moved"] != false {
		t.Fatalf("新建卡片不该再走一次流转：%v", body["moved"])
	}

	id := strconv.Itoa(int(app["id"].(float64)))
	_, detail := do(t, r, http.MethodGet, "/api/applications/"+id, nil)
	for _, e := range detail["events"].([]any) {
		if strings.Contains(e.(map[string]any)["text"].(string), "推进到") {
			t.Fatalf("不该有流转日志：%v", detail["events"])
		}
	}
}

// 起始阶段可以自选，但「必须先有跟进人」这条规则照样要守。
func TestCreateAtStageStillRequiresOwner(t *testing.T) {
	r := setup(t)

	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "没跟进人公司", "stage_key": "round_1"})
	if code != http.StatusConflict || errCode(t, body) != "OWNER_REQUIRED" {
		t.Fatalf("状态码 = %d，body = %v", code, body)
	}

	code, body = do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "有跟进人公司", "stage_key": "round_1", "owner_id": 1})
	if code != http.StatusCreated {
		t.Fatalf("指定跟进人后应当成功：%d %v", code, body)
	}
	if got := body["application"].(map[string]any)["stage_key"]; got != "round_1" {
		t.Fatalf("起始阶段 = %v，期望 round_1", got)
	}

	// 编出来的阶段仍然要被拒。
	code, body = do(t, r, http.MethodPost, "/api/boards/JOBHUNT/applications",
		map[string]any{"company": "乱填阶段公司", "stage_key": "nope"})
	if code != http.StatusBadRequest {
		t.Fatalf("未知阶段应当返回 400：%d %v", code, body)
	}
}

// AI 录入是本人在录自己的面试，跟进人默认落在当前用户身上。
func TestIngestConfirmDefaultsOwnerToActor(t *testing.T) {
	r := setupWithModel(t, &fakeModel{reply: foodpandaReply})

	// 不传 owner_id：兜底成当前用户，「一面」的 requires_owner 也就自然满足了。
	code, body := do(t, r, http.MethodPost, "/api/boards/JOBHUNT/ingest/confirm", map[string]any{
		"company":   "默认跟进人公司",
		"stage_key": "round_1",
	})
	if code != http.StatusCreated {
		t.Fatalf("确认录入返回 %d：%v", code, body)
	}
	app := body["application"].(map[string]any)
	if app["owner_id"] == nil {
		t.Fatalf("跟进人应当默认是当前用户：%v", app)
	}
	if got := app["owner"].(map[string]any)["name"]; got != "我" {
		t.Fatalf("跟进人 = %v，期望「我」", got)
	}

	// 显式传 0 表示「就是不要跟进人」，不该被默认值盖掉。
	code, body = do(t, r, http.MethodPost, "/api/boards/JOBHUNT/ingest/confirm", map[string]any{
		"company":   "不要跟进人公司",
		"owner_id":  0,
		"stage_key": "online_test",
	})
	if code != http.StatusCreated {
		t.Fatalf("确认录入返回 %d：%v", code, body)
	}
	if got := body["application"].(map[string]any)["owner_id"]; got != nil {
		t.Fatalf("显式选了「未指定」，不该被兜底成当前用户：%v", got)
	}
}
