package application

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"interview/internal/board"
	"interview/internal/member"
	"interview/internal/platform/apperr"
	"interview/internal/platform/db/dbtest"
	"interview/internal/stage"
	"interview/internal/workflow"
)

// setup 起一个临时库，写入一个看板、一套阶段和两名成员。
func setup(t *testing.T) (*Service, int64, []int64) {
	t.Helper()

	conn := dbtest.New(t)

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := conn.Exec(`INSERT INTO boards ("key", name, description, created_at) VALUES ('T', '测试看板', '', ?)`, now)
	if err != nil {
		t.Fatalf("写入测试看板失败: %v", err)
	}
	boardID, _ := res.LastInsertId()

	seedStages(t, conn, boardID)

	members := member.NewService(conn)
	ids := make([]int64, 0, 2)
	for _, name := range []string{"甲", "乙"} {
		m, err := members.Create(context.Background(), name, "member")
		if err != nil {
			t.Fatalf("写入成员失败: %v", err)
		}
		ids = append(ids, m.ID)
	}

	stages := stage.NewService(conn)
	return NewService(conn, members, board.NewService(conn), stages), boardID, ids
}

// seedStages 直接写表，位置显式给定，测试不依赖 stage.Create 的默认插入策略。
func seedStages(t *testing.T, db *sql.DB, boardID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	rows := []struct {
		key, label, kind string
		requiresOwner    int
	}{
		{"hr_screen", "HR screen", string(workflow.KindNormal), 0},
		{"online_test", "在线测评", string(workflow.KindNormal), 0},
		{"round_1", "一面", string(workflow.KindInterview), 1},
		{"waiting_reply", "等待回复中", string(workflow.KindWaiting), 0},
		{"offer", "已发 Offer", string(workflow.KindOffer), 0},
		{"rejected", "已结束", string(workflow.KindRejected), 0},
	}
	for i, r := range rows {
		if _, err := db.Exec(`
			INSERT INTO stages (board_id, "key", label, kind, color, requires_owner, position, created_at)
			VALUES (?, ?, ?, ?, '#888888', ?, ?, ?)`,
			boardID, r.key, r.label, r.kind, r.requiresOwner, float64((i+1)*1000), now); err != nil {
			t.Fatalf("写入阶段失败: %v", err)
		}
	}
}

func mustCreate(t *testing.T, svc *Service, boardID int64, in CreateInput) Application {
	t.Helper()
	created, err := svc.Create(context.Background(), boardID, in, 0)
	if err != nil {
		t.Fatalf("创建流程失败: %v", err)
	}
	return created
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("期望 apperr，实际 %v", err)
	}
	return appErr.Code
}

func TestCreateValidation(t *testing.T) {
	svc, boardID, _ := setup(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, boardID, CreateInput{Company: "   "}, 0); err == nil {
		t.Fatal("空公司名应当被拒绝")
	}
	if _, err := svc.Create(ctx, boardID, CreateInput{Company: "某公司", Intent: "urgent"}, 0); err == nil {
		t.Fatal("非法意向度应当被拒绝")
	}

	created := mustCreate(t, svc, boardID, CreateInput{Company: "某公司"})
	if created.StageKey != "hr_screen" {
		t.Fatalf("新流程应落在首列，实际 %s", created.StageKey)
	}
	if created.Key != "T-1" {
		t.Fatalf("展示编号 = %s，期望 T-1", created.Key)
	}
}

func TestMoveEnforcesStageRules(t *testing.T) {
	svc, boardID, members := setup(t)
	ctx := context.Background()
	app := mustCreate(t, svc, boardID, CreateInput{Company: "某公司"})

	if _, err := svc.Move(ctx, app.ID, "round_1", -1, 0); codeOf(t, err) != apperr.CodeInvalidTransition {
		t.Fatalf("跨阶段跳跃应当被拒绝，实际 %v", err)
	}

	moved, err := svc.Move(ctx, app.ID, "online_test", -1, 0)
	if err != nil {
		t.Fatalf("相邻推进失败: %v", err)
	}
	if moved.StageLabel != "在线测评" {
		t.Fatalf("阶段 = %s，期望 在线测评", moved.StageLabel)
	}

	// 一面配了 requires_owner，没有跟进人时进不去。
	if _, err := svc.Move(ctx, app.ID, "round_1", -1, 0); codeOf(t, err) != apperr.CodeOwnerRequired {
		t.Fatalf("期望 OWNER_REQUIRED，实际 %v", err)
	}
	if _, err := svc.Update(ctx, app.ID, UpdateInput{OwnerID: &members[0]}, 0); err != nil {
		t.Fatalf("指定跟进人失败: %v", err)
	}
	if _, err := svc.Move(ctx, app.ID, "round_1", -1, 0); err != nil {
		t.Fatalf("指定跟进人后应当放行: %v", err)
	}

	// 任何阶段都能直达终态。
	done, err := svc.Move(ctx, app.ID, "rejected", -1, 0)
	if err != nil {
		t.Fatalf("直达终态失败: %v", err)
	}
	if done.StageKind != workflow.KindRejected {
		t.Fatalf("阶段类型 = %s，期望 %s", done.StageKind, workflow.KindRejected)
	}
}

func TestMoveIntoInterviewStageCreatesPendingRound(t *testing.T) {
	svc, boardID, members := setup(t)
	ctx := context.Background()
	app := mustCreate(t, svc, boardID, CreateInput{Company: "某公司", OwnerID: &members[0]})

	if _, err := svc.Move(ctx, app.ID, "online_test", -1, 0); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	moved, err := svc.Move(ctx, app.ID, "round_1", -1, 0)
	if err != nil {
		t.Fatalf("推进失败: %v", err)
	}

	if moved.RoundCount != 1 {
		t.Fatalf("面试轮次 = %d，期望自动建 1 条", moved.RoundCount)
	}
	if moved.NextRound == nil || moved.NextRound.ScheduledAt != nil {
		t.Fatal("自动创建的轮次应当是「待安排」")
	}
	if !moved.NeedsSchedule() {
		t.Fatal("停在面试阶段且未排期，应当提示去安排时间")
	}

	// 同列内拖动不应重复建记录。
	again, err := svc.Move(ctx, app.ID, "round_1", 0, 0)
	if err != nil {
		t.Fatalf("同列排序失败: %v", err)
	}
	if again.RoundCount != 1 {
		t.Fatalf("同列拖动后轮次 = %d，期望仍为 1", again.RoundCount)
	}
}

func TestScheduleAndUpdateRound(t *testing.T) {
	svc, boardID, members := setup(t)
	ctx := context.Background()
	app := mustCreate(t, svc, boardID, CreateInput{Company: "某公司", OwnerID: &members[0]})

	at := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	round, err := svc.ScheduleRound(ctx, app.ID, RoundInput{
		ScheduledAt: &at,
		Mode:        ModeOnline,
		MeetingURL:  "https://meeting.example.com/abc",
		Interviewer: "张工",
	}, 0)
	if err != nil {
		t.Fatalf("安排面试失败: %v", err)
	}
	if round.DurationMin != 60 {
		t.Fatalf("默认时长 = %d，期望 60", round.DurationMin)
	}
	if !round.Scheduled() || round.Overdue() {
		t.Fatal("未来的面试应当是已排期且未逾期")
	}

	// 卡片上直接能看到下一场面试。
	reloaded, err := svc.Get(ctx, app.ID)
	if err != nil {
		t.Fatalf("读取流程失败: %v", err)
	}
	if reloaded.NextRound == nil || reloaded.NextRound.MeetingURL != "https://meeting.example.com/abc" {
		t.Fatalf("卡片未带出会议信息: %+v", reloaded.NextRound)
	}

	passed := ResultPassed
	place := "总部 A 座会议室"
	updated, err := svc.UpdateRound(ctx, round.ID, RoundUpdateInput{Result: &passed, MeetingPlace: &place}, 0)
	if err != nil {
		t.Fatalf("回填结果失败: %v", err)
	}
	if updated.Result != ResultPassed || updated.MeetingPlace != place {
		t.Fatalf("回填后 = %+v", updated)
	}

	// 结果已回填，卡片上不再有「下一场」。
	after, _ := svc.Get(ctx, app.ID)
	if after.NextRound != nil {
		t.Fatalf("已通过的轮次不应再作为下一场：%+v", after.NextRound)
	}

	if _, err := svc.DeleteRound(ctx, round.ID, 0); err != nil {
		t.Fatalf("删除面试记录失败: %v", err)
	}
	detail, _ := svc.GetDetail(ctx, app.ID)
	if len(detail.Rounds) != 0 {
		t.Fatalf("删除后仍有 %d 条记录", len(detail.Rounds))
	}
}

func TestDuplicateCompanyAndRole(t *testing.T) {
	svc, boardID, _ := setup(t)
	ctx := context.Background()
	mustCreate(t, svc, boardID, CreateInput{Company: "某公司", Role: "后端研发"})

	// 公司 + 岗位都一样 → 409。
	_, err := svc.Create(ctx, boardID, CreateInput{Company: "某公司", Role: "后端研发"}, 0)
	if codeOf(t, err) != apperr.CodeConflict {
		t.Fatalf("重复投递应当返回 CONFLICT，实际 %v", err)
	}
	// 大小写与首尾空格不算新公司。
	_, err = svc.Create(ctx, boardID, CreateInput{Company: "  某公司 ", Role: "后端研发"}, 0)
	if codeOf(t, err) != apperr.CodeConflict {
		t.Fatalf("去空格后重复应当返回 CONFLICT，实际 %v", err)
	}
	// 同公司不同岗位允许。
	other := mustCreate(t, svc, boardID, CreateInput{Company: "某公司", Role: "前端研发"})

	// 改名撞车同样被拦。
	role := "后端研发"
	if _, err := svc.Update(ctx, other.ID, UpdateInput{Role: &role}, 0); codeOf(t, err) != apperr.CodeConflict {
		t.Fatalf("改岗位撞车应当返回 CONFLICT，实际 %v", err)
	}
	// 改成别的岗位没问题，也不会把自己判成重复。
	free := "测试开发"
	if _, err := svc.Update(ctx, other.ID, UpdateInput{Role: &free}, 0); err != nil {
		t.Fatalf("改成空闲岗位失败: %v", err)
	}
	// 不碰公司 / 岗位的更新不受影响。
	notes := "面完补充"
	if _, err := svc.Update(ctx, other.ID, UpdateInput{Notes: &notes}, 0); err != nil {
		t.Fatalf("只改备注失败: %v", err)
	}
}

func TestDeleteCascadesRoundsAndEvents(t *testing.T) {
	svc, boardID, _ := setup(t)
	ctx := context.Background()
	app := mustCreate(t, svc, boardID, CreateInput{Company: "某公司"})

	if _, err := svc.ScheduleRound(ctx, app.ID, RoundInput{}, 0); err != nil {
		t.Fatalf("安排面试失败: %v", err)
	}

	if err := svc.Delete(ctx, app.ID); err != nil {
		t.Fatalf("删除流程失败: %v", err)
	}
	if _, err := svc.Get(ctx, app.ID); codeOf(t, err) != apperr.CodeNotFound {
		t.Fatalf("删除后再查应当返回 NOT_FOUND，实际 %v", err)
	}

	// 面试记录与操作日志靠外键级联清除；这里直接查库，顺带验证 foreign_keys pragma 确实开着。
	for _, table := range []string{"interview_rounds", "application_events"} {
		var n int
		if err := svc.repo.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE application_id = ?`, app.ID).Scan(&n); err != nil {
			t.Fatalf("统计 %s 失败: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s 还剩 %d 行，级联删除没生效", table, n)
		}
	}
}

func TestDeleteMissing(t *testing.T) {
	svc, _, _ := setup(t)
	if err := svc.Delete(context.Background(), 999999); codeOf(t, err) != apperr.CodeNotFound {
		t.Fatalf("删除不存在的流程应当返回 NOT_FOUND，实际 %v", err)
	}
}

func TestUpdateNoChangeSkipsLog(t *testing.T) {
	svc, boardID, _ := setup(t)
	ctx := context.Background()
	app := mustCreate(t, svc, boardID, CreateInput{Company: "某公司"})

	before, _ := svc.GetDetail(ctx, app.ID)
	same := "某公司"
	if _, err := svc.Update(ctx, app.ID, UpdateInput{Company: &same}, 0); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	after, _ := svc.GetDetail(ctx, app.ID)
	if len(after.Events) != len(before.Events) {
		t.Fatalf("无变化不应写日志：%d → %d", len(before.Events), len(after.Events))
	}
}

func TestFilterByStageAndOwner(t *testing.T) {
	svc, boardID, members := setup(t)
	ctx := context.Background()

	a := mustCreate(t, svc, boardID, CreateInput{Company: "甲公司", OwnerID: &members[0]})
	mustCreate(t, svc, boardID, CreateInput{Company: "乙公司"})
	if _, err := svc.Move(ctx, a.ID, "online_test", -1, 0); err != nil {
		t.Fatalf("推进失败: %v", err)
	}

	key := "online_test"
	byStage, err := svc.List(ctx, boardID, Filter{StageKey: &key})
	if err != nil {
		t.Fatalf("按阶段筛选失败: %v", err)
	}
	if len(byStage) != 1 || byStage[0].Company != "甲公司" {
		t.Fatalf("按阶段筛选结果 = %+v", byStage)
	}

	var unassigned int64
	byOwner, err := svc.List(ctx, boardID, Filter{OwnerID: &unassigned})
	if err != nil {
		t.Fatalf("按跟进人筛选失败: %v", err)
	}
	if len(byOwner) != 1 || byOwner[0].Company != "乙公司" {
		t.Fatalf("未指定跟进人的结果 = %+v", byOwner)
	}

	upcoming, err := svc.List(ctx, boardID, Filter{Upcoming: true})
	if err != nil {
		t.Fatalf("按排期筛选失败: %v", err)
	}
	if len(upcoming) != 0 {
		t.Fatalf("还没有排期，期望 0 条，实际 %d", len(upcoming))
	}
}

// 面完了在等结果：卡片上还看得见这一轮，但它不再算「还有事要做」——
// 既不催去约时间，也不算逾期，「只看已排期」里也不该出现。
func TestAwaitingResultLeavesTheQueue(t *testing.T) {
	svc, boardID, members := setup(t)
	ctx := context.Background()
	app := mustCreate(t, svc, boardID, CreateInput{Company: "某公司", OwnerID: &members[0]})
	if _, err := svc.Move(ctx, app.ID, "online_test", -1, 0); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	if _, err := svc.Move(ctx, app.ID, "round_1", -1, 0); err != nil {
		t.Fatalf("推进失败: %v", err)
	}

	past := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	detail, err := svc.GetDetail(ctx, app.ID)
	if err != nil {
		t.Fatalf("读取详情失败: %v", err)
	}
	roundID := detail.Rounds[0].ID
	if _, err := svc.UpdateRound(ctx, roundID, RoundUpdateInput{ScheduledAt: &past}, 0); err != nil {
		t.Fatalf("排期失败: %v", err)
	}

	// 时间过了、结果还挂着「待进行」——这才是要催的状态。
	overdue, _ := svc.Get(ctx, app.ID)
	if overdue.NextRound == nil || !overdue.NextRound.Overdue() {
		t.Fatalf("面试时间已过却没标成逾期：%+v", overdue.NextRound)
	}

	awaiting := ResultAwaiting
	updated, err := svc.UpdateRound(ctx, roundID, RoundUpdateInput{Result: &awaiting}, 0)
	if err != nil {
		t.Fatalf("标记等结果失败: %v", err)
	}
	if !updated.Awaiting() || updated.Overdue() {
		t.Fatalf("标记等结果后 = %+v", updated)
	}

	after, _ := svc.Get(ctx, app.ID)
	if after.NextRound == nil || after.NextRound.ID != roundID {
		t.Fatalf("等结果的那一轮应当还留在卡片上：%+v", after.NextRound)
	}
	if after.NeedsSchedule() {
		t.Fatal("面都面完了，不该再催去安排时间")
	}

	upcoming, err := svc.List(ctx, boardID, Filter{Upcoming: true})
	if err != nil {
		t.Fatalf("按排期筛选失败: %v", err)
	}
	if len(upcoming) != 0 {
		t.Fatalf("等结果的不算「已排期」，期望 0 条，实际 %d", len(upcoming))
	}

	// 又约上了下一场：卡片要让位给还没进行的那一场。
	next := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	fresh, err := svc.ScheduleRound(ctx, app.ID, RoundInput{ScheduledAt: &next}, 0)
	if err != nil {
		t.Fatalf("安排下一场失败: %v", err)
	}
	reloaded, _ := svc.Get(ctx, app.ID)
	if reloaded.NextRound == nil || reloaded.NextRound.ID != fresh.ID {
		t.Fatalf("卡片上应当是新约的那一场：%+v", reloaded.NextRound)
	}
}

// 挪进「等待回复中」= 那一轮面完了在等通知：记录留着，但不再是待办。
func TestMoveIntoWaitingStageClosesRound(t *testing.T) {
	svc, boardID, members := setup(t)
	ctx := context.Background()
	app := mustCreate(t, svc, boardID, CreateInput{Company: "某公司", OwnerID: &members[0]})
	if _, err := svc.Move(ctx, app.ID, "online_test", -1, 0); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	if _, err := svc.Move(ctx, app.ID, "round_1", -1, 0); err != nil {
		t.Fatalf("推进失败: %v", err)
	}

	// 一面已经面过了，只是结果还没回填。
	past := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	detail, _ := svc.GetDetail(ctx, app.ID)
	roundID := detail.Rounds[0].ID
	if _, err := svc.UpdateRound(ctx, roundID, RoundUpdateInput{ScheduledAt: &past}, 0); err != nil {
		t.Fatalf("排期失败: %v", err)
	}

	moved, err := svc.Move(ctx, app.ID, "waiting_reply", -1, 0)
	if err != nil {
		t.Fatalf("挪进等待回复列失败: %v", err)
	}

	after, err := svc.GetDetail(ctx, app.ID)
	if err != nil {
		t.Fatalf("读取详情失败: %v", err)
	}
	if len(after.Rounds) != 1 {
		t.Fatalf("轮次数 = %d，等待回复列不该再挂一条待办", len(after.Rounds))
	}
	if after.Rounds[0].Result != ResultAwaiting {
		t.Fatalf("一面的结果 = %q，期望自动收尾成 %q", after.Rounds[0].Result, ResultAwaiting)
	}
	if after.Rounds[0].Overdue() {
		t.Fatal("面完在等通知，不该再算逾期")
	}
	if moved.NeedsSchedule() {
		t.Fatal("等待回复列不该催排期")
	}
	// 面试记录本身留着——时间、面试官、评价都还要回头看。
	if after.Rounds[0].ScheduledAt == nil || !after.Rounds[0].ScheduledAt.Equal(past.UTC()) {
		t.Fatalf("面试时间被动了: %+v", after.Rounds[0].ScheduledAt)
	}

	// 收尾这件事写进操作日志，不是悄悄改的。
	found := false
	for _, e := range after.Events {
		if strings.Contains(e.Detail, "等结果") {
			found = true
		}
	}
	if !found {
		t.Fatal("自动收尾没有留下操作日志")
	}

	// 通知来了，从这一列可以直接回到面试阶段，照常挂一条新的待安排。
	back, err := svc.Move(ctx, app.ID, "round_1", -1, 0)
	if err != nil {
		t.Fatalf("从等待回复列回到一面失败: %v", err)
	}
	if back.RoundCount != 2 || back.NextRound == nil || back.NextRound.ScheduledAt != nil {
		t.Fatalf("回到面试阶段应当新挂一条待安排: count=%d next=%+v", back.RoundCount, back.NextRound)
	}
}
