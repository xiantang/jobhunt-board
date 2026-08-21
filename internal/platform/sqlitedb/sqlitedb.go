// Package sqlitedb 负责 SQLite 连接、表结构迁移与首次启动的种子数据。
package sqlitedb

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// Open 打开（必要时创建）数据库文件，设置 PRAGMA 并执行迁移。
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// SQLite 写入串行化，限制单连接可以彻底规避 database is locked。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate 执行幂等的建表语句。
func Migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("初始化表结构失败: %w", err)
	}
	return nil
}

// defaultStages 是新看板的默认面试流程。用户可以在页面上随意增删改序，
// 这里只提供一个开箱即用的起点。
var defaultStages = []struct {
	key, label, kind, color string
	requiresOwner           bool
}{
	{"hr_screen", "HR 初筛", "normal", "#64748b", false},
	{"online_test", "在线测评", "normal", "#0891b2", false},
	{"round_1", "一面", "interview", "#2563eb", true},
	{"round_2", "二面", "interview", "#4f46e5", false},
	{"round_3", "三面", "interview", "#7c3aed", false},
	{"round_4", "四面", "interview", "#a21caf", false},
	{"hrbp", "HRBP 面", "interview", "#db2777", false},
	{"offer_wait", "Offer 审批中", "normal", "#d97706", false},
	{"offer", "已发 Offer", "terminal_success", "#16a34a", false},
	{"rejected", "已结束", "terminal_fail", "#6b7280", false},
}

// Seed 仅在库为空时写入演示数据：一个看板、十个阶段、三名成员、五条面试流程。
func Seed(db *sql.DB) error {
	var boards int
	if err := db.QueryRow(`SELECT COUNT(*) FROM boards`).Scan(&boards); err != nil {
		return fmt.Errorf("检查种子数据失败: %w", err)
	}
	if boards > 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	res, err := tx.Exec(`INSERT INTO boards (key, name, description, created_at) VALUES (?, ?, ?, ?)`,
		"JOBHUNT", "面试流程看板", "记录每家公司的面试进度、面试时间与会议信息", nowStr)
	if err != nil {
		return fmt.Errorf("写入种子看板失败: %w", err)
	}
	boardID, _ := res.LastInsertId()

	for i, s := range defaultStages {
		requires := 0
		if s.requiresOwner {
			requires = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO stages (board_id, key, label, kind, color, requires_owner, position, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			boardID, s.key, s.label, s.kind, s.color, requires, float64((i+1)*1000), nowStr); err != nil {
			return fmt.Errorf("写入种子阶段失败: %w", err)
		}
	}

	members := []struct{ name, role, color string }{
		{"A", "lead", "#2563eb"},
		{"B", "member", "#16a34a"},
		{"C", "member", "#d97706"},
	}
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		r, err := tx.Exec(`INSERT INTO members (name, role, color, created_at) VALUES (?, ?, ?, ?)`,
			m.name, m.role, m.color, nowStr)
		if err != nil {
			return fmt.Errorf("写入种子成员失败: %w", err)
		}
		id, _ := r.LastInsertId()
		ids = append(ids, id)
	}

	applications := []struct {
		company, role, channel, notes, stage, intent string
		owner                                        int64
	}{
		{"百度", "后端研发工程师", "内推", "内推码已用，简历已过初筛", "round_2", "high", ids[0]},
		{"某电商", "Go 开发", "官网投递", "在线测评 90 分钟，限时", "online_test", "normal", ids[1]},
		{"某创业公司", "基础架构", "猎头", "HRBP 沟通薪资中", "hrbp", "high", ids[0]},
		{"某外企", "平台工程", "官网投递", "面试官反馈方向不匹配", "rejected", "low", ids[2]},
		{"某游戏公司", "服务端开发", "内推", "一面还没约上时间", "round_1", "normal", ids[1]},
	}
	appIDs := make([]int64, 0, len(applications))
	for i, a := range applications {
		r, err := tx.Exec(`
			INSERT INTO applications (board_id, seq, company, role, channel, notes, stage_key, intent, owner_id, position, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			boardID, i+1, a.company, a.role, a.channel, a.notes, a.stage, a.intent, a.owner,
			float64((i+1)*1000), nowStr, nowStr)
		if err != nil {
			return fmt.Errorf("写入种子流程失败: %w", err)
		}
		id, _ := r.LastInsertId()
		appIDs = append(appIDs, id)
	}

	// 四条示例排期，覆盖「已通过」「未来已排期」「已过期待回填」「待安排」四种卡片形态。
	inTwoDays := now.AddDate(0, 0, 2).Format(time.RFC3339)
	twoDaysAgo := now.AddDate(0, 0, -2).Format(time.RFC3339)
	rounds := []struct {
		app                                                         int64
		stageKey, stageLabel, mode, url, place, interviewer, result string
		scheduled                                                   any
	}{
		{appIDs[0], "round_1", "一面", "online", "https://meeting.example.com/abc-123", "", "张工", "passed", twoDaysAgo},
		{appIDs[0], "round_2", "二面", "online", "https://meeting.example.com/def-456", "", "李工", "pending", inTwoDays},
		{appIDs[2], "hrbp", "HRBP 面", "onsite", "", "总部 A 座 12 层会议室", "王 HRBP", "pending", twoDaysAgo},
		{appIDs[4], "round_1", "一面", "online", "", "", "", "pending", nil}, // 待安排：卡片上会标黄提醒
	}
	for _, r := range rounds {
		if _, err := tx.Exec(`
			INSERT INTO interview_rounds (application_id, stage_key, stage_label, scheduled_at, duration_min,
			                              mode, meeting_url, meeting_place, interviewer, result, notes, created_at, updated_at)
			VALUES (?, ?, ?, ?, 60, ?, ?, ?, ?, ?, '', ?, ?)`,
			r.app, r.stageKey, r.stageLabel, r.scheduled, r.mode, r.url, r.place, r.interviewer, r.result,
			nowStr, nowStr); err != nil {
			return fmt.Errorf("写入种子面试记录失败: %w", err)
		}
	}

	return tx.Commit()
}
