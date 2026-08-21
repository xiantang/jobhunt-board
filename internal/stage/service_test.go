package stage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"interview/internal/platform/apperr"
	"interview/internal/platform/sqlitedb"
	"interview/internal/workflow"
)

// setup 起一个临时库，写入一个空看板，返回服务与看板 ID。
func setup(t *testing.T) (*Service, *sql.DB, int64) {
	t.Helper()

	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(`INSERT INTO boards (key, name, description, created_at) VALUES ('T', '测试看板', '', ?)`, now)
	if err != nil {
		t.Fatalf("写入测试看板失败: %v", err)
	}
	boardID, _ := res.LastInsertId()
	return NewService(db), db, boardID
}

// seedStages 写入一套最小可用的阶段：两个普通阶段 + 两个终态。
func seedStages(t *testing.T, s *Service, boardID int64) {
	t.Helper()
	ctx := context.Background()
	for _, in := range []CreateInput{
		{Label: "HR screen", Kind: string(workflow.KindNormal), Index: -1},
		{Label: "Round 1", Kind: string(workflow.KindInterview), Index: -1},
		{Label: "Offer", Kind: string(workflow.KindOffer), Index: -1},
		{Label: "Rejected", Kind: string(workflow.KindRejected), Index: -1},
	} {
		if _, err := s.Create(ctx, boardID, in); err != nil {
			t.Fatalf("写入阶段 %s 失败: %v", in.Label, err)
		}
	}
}

func labels(stages []Stage) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		out = append(out, s.Label)
	}
	return out
}

func TestCreateInsertsBeforeTerminals(t *testing.T) {
	svc, _, boardID := setup(t)
	seedStages(t, svc, boardID)
	ctx := context.Background()

	if _, err := svc.Create(ctx, boardID, CreateInput{Label: "HRBP", Index: -1}); err != nil {
		t.Fatalf("新增阶段失败: %v", err)
	}
	stages, err := svc.List(ctx, boardID)
	if err != nil {
		t.Fatalf("读取阶段失败: %v", err)
	}
	want := []string{"HR screen", "Round 1", "HRBP", "Offer", "Rejected"}
	for i, l := range want {
		if labels(stages)[i] != l {
			t.Fatalf("列顺序 = %v，期望 %v", labels(stages), want)
		}
	}
}

func TestCreateKeyIsUnique(t *testing.T) {
	svc, _, boardID := setup(t)
	ctx := context.Background()

	first, err := svc.Create(ctx, boardID, CreateInput{Label: "一面", Index: -1})
	if err != nil {
		t.Fatalf("新增阶段失败: %v", err)
	}
	second, err := svc.Create(ctx, boardID, CreateInput{Label: "二面", Index: -1})
	if err != nil {
		t.Fatalf("新增阶段失败: %v", err)
	}
	// 中文标签取不出 ASCII slug，回退到 stage 并自动补序号。
	if first.Key == second.Key {
		t.Fatalf("两个阶段的 key 重复: %s", first.Key)
	}
}

func TestUpdateKeepsKey(t *testing.T) {
	svc, _, boardID := setup(t)
	seedStages(t, svc, boardID)
	ctx := context.Background()

	stages, _ := svc.List(ctx, boardID)
	target := stages[0]

	label := "简历初筛"
	updated, err := svc.Update(ctx, target.ID, UpdateInput{Label: &label})
	if err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if updated.Label != label {
		t.Fatalf("展示名 = %s，期望 %s", updated.Label, label)
	}
	if updated.Key != target.Key {
		t.Fatalf("key 应当保持不变，%s → %s", target.Key, updated.Key)
	}
}

func TestReorder(t *testing.T) {
	svc, _, boardID := setup(t)
	seedStages(t, svc, boardID)
	ctx := context.Background()

	stages, _ := svc.List(ctx, boardID)
	if _, err := svc.Reorder(ctx, stages[1].ID, 0); err != nil {
		t.Fatalf("调序失败: %v", err)
	}
	after, _ := svc.List(ctx, boardID)
	if after[0].Label != "Round 1" {
		t.Fatalf("调序后首列 = %s，期望 Round 1", after[0].Label)
	}
}

func TestDeleteRejectsNonEmptyStage(t *testing.T) {
	svc, db, boardID := setup(t)
	seedStages(t, svc, boardID)
	ctx := context.Background()

	stages, _ := svc.List(ctx, boardID)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`
		INSERT INTO applications (board_id, seq, company, stage_key, position, created_at, updated_at)
		VALUES (?, 1, '某公司', ?, 1000, ?, ?)`, boardID, stages[0].Key, now, now); err != nil {
		t.Fatalf("写入流程失败: %v", err)
	}

	err := svc.Delete(ctx, stages[0].ID)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Code != apperr.CodeConflict {
		t.Fatalf("期望 CONFLICT，实际 %v", err)
	}
}

func TestDeleteKeepsOneActiveStage(t *testing.T) {
	svc, _, boardID := setup(t)
	ctx := context.Background()

	only, err := svc.Create(ctx, boardID, CreateInput{Label: "Screening", Index: -1})
	if err != nil {
		t.Fatalf("新增阶段失败: %v", err)
	}
	if _, err := svc.Create(ctx, boardID, CreateInput{Label: "Offer", Kind: string(workflow.KindOffer), Index: -1}); err != nil {
		t.Fatalf("新增阶段失败: %v", err)
	}

	if err := svc.Delete(ctx, only.ID); err == nil {
		t.Fatal("删掉最后一个非终态阶段应当被拒绝")
	}
	// 改成终态同样会让看板没有落脚点。
	kind := string(workflow.KindRejected)
	if _, err := svc.Update(ctx, only.ID, UpdateInput{Kind: &kind}); err == nil {
		t.Fatal("把最后一个非终态阶段改成终态应当被拒绝")
	}
}

func TestFlowMirrorsConfiguredOrder(t *testing.T) {
	svc, _, boardID := setup(t)
	seedStages(t, svc, boardID)
	ctx := context.Background()

	flow, err := svc.FlowOf(ctx, boardID)
	if err != nil {
		t.Fatalf("构造流转规则失败: %v", err)
	}
	from, err := flow.Parse(flow.Stages()[0].Key)
	if err != nil {
		t.Fatalf("解析阶段失败: %v", err)
	}
	entry, err := flow.Entry()
	if err != nil {
		t.Fatalf("取首列失败: %v", err)
	}
	if entry.Key != from.Key {
		t.Fatalf("首列 = %s，期望 %s", entry.Key, from.Key)
	}
}
