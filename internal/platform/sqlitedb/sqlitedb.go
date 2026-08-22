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

// Migrate 执行幂等的建表语句，补上建表之后新增的列，再跑一次性的数据修补。
func Migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("初始化表结构失败: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS 不会给已存在的表加列，旧库要单独补。
	if _, err := addColumn(db, "stages", "skippable", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := once(db, "skippable_defaults", backfillSkippable); err != nil {
		return err
	}
	return once(db, "solo_member", collapseSeedMembers)
}

// SoloMemberName 是默认成员名——这个看板是一个人在用，操作日志和卡片头像都显示它。
const SoloMemberName = "我"

// collapseSeedMembers 把老库里的演示成员收成一个人。
//
// 早期种子写了 A / B / C 三个人，但这是个人求职看板，A 就是本人。
// 这里把 A 改名成「我」，B / C 没有任何流程和日志时删掉——
// 有引用就留着，宁可界面上多两个名字，也不要把历史记录的作者抹成空。
func collapseSeedMembers(db *sql.DB) error {
	// 只在成员表看起来还是那份种子数据时动手，免得误伤真给成员起名叫 A 的库。
	var total, seeded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM members`).Scan(&total); err != nil {
		return fmt.Errorf("统计成员失败: %w", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM members WHERE name IN ('A', 'B', 'C')`).Scan(&seeded); err != nil {
		return fmt.Errorf("统计种子成员失败: %w", err)
	}
	if total != 3 || seeded != 3 {
		return nil
	}

	if _, err := db.Exec(`UPDATE members SET name = ?, role = 'lead' WHERE name = 'A'`, SoloMemberName); err != nil {
		return fmt.Errorf("重命名成员 A 失败: %w", err)
	}
	// 只删真正没被用过的：owner_id 是 ON DELETE SET NULL，
	// 直接删会把卡片的跟进人和日志作者悄悄清空。
	if _, err := db.Exec(`
		DELETE FROM members
		WHERE name IN ('B', 'C')
		  AND id NOT IN (SELECT owner_id FROM applications WHERE owner_id IS NOT NULL)
		  AND id NOT IN (SELECT actor_id FROM application_events WHERE actor_id IS NOT NULL)`); err != nil {
		return fmt.Errorf("删除未使用的种子成员失败: %w", err)
	}
	return nil
}

// once 保证一段数据修补在同一个库上只跑一次，跑过的记在 migrations 表里。
//
// 「列在不在」不足以判断修补做没做——列可能是上个版本加的，
// 那时还没有这段回填逻辑。用一张表显式记录，才不会漏掉中间状态的库。
func once(db *sql.DB, id string, fn func(*sql.DB) error) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS migrations (
		id         TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("初始化 migrations 表失败: %w", err)
	}

	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE id = ?`, id).Scan(&applied); err != nil {
		return fmt.Errorf("检查迁移 %s 是否已执行失败: %w", id, err)
	}
	if applied > 0 {
		return nil
	}
	if err := fn(db); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO migrations (id, applied_at) VALUES (?, ?)`,
		id, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("记录迁移 %s 失败: %w", id, err)
	}
	return nil
}

// backfillSkippable 给已有的库补上「可跳过」的默认值。
//
// ALTER TABLE 只能给一个统一的 DEFAULT 0，结果是旧库里每一列都成了「不可跳过」，
// 和新建库不一致——「在线测评」不可跳过，HR screen 就没法直达一面。
// 这里按 key 把默认配置里该开的开上。它只跑一次，
// 之后用户在页面上取消勾选，重启也不会被重新打开。
func backfillSkippable(db *sql.DB) error {
	for _, s := range defaultStages {
		if !s.skippable {
			continue
		}
		if _, err := db.Exec(`UPDATE stages SET skippable = 1 WHERE key = ?`, s.key); err != nil {
			return fmt.Errorf("回填 %s 的 skippable 失败: %w", s.key, err)
		}
	}
	return nil
}

// addColumn 幂等地给表加一列，返回这次是否真的加了。列已存在就跳过。
func addColumn(db *sql.DB, table, column, decl string) (bool, error) {
	rows, err := db.Query(`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("检查 %s.%s 是否存在失败: %w", table, column, err)
	}
	exists := rows.Next()
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("检查 %s.%s 是否存在失败: %w", table, column, err)
	}
	rows.Close()
	if exists {
		return false, nil
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)); err != nil {
		return false, fmt.Errorf("给 %s 添加 %s 列失败: %w", table, column, err)
	}
	return true, nil
}

// defaultStages 是新看板的默认面试流程。用户可以在页面上随意增删改序，
// 这里只提供一个开箱即用的起点。
var defaultStages = []struct {
	key, label, kind, color string
	requiresOwner           bool
	skippable               bool
}{
	{"hr_screen", "HR screen", "normal", "#64748b", false, false},
	// 不是每家公司都有在线测评，默认可跳过：HR screen 能直达一面。
	{"online_test", "在线测评", "normal", "#0891b2", false, true},
	{"round_1", "一面", "interview", "#2563eb", true, false},
	{"round_2", "二面", "interview", "#4f46e5", false, false},
	{"round_3", "三面", "interview", "#7c3aed", false, false},
	{"round_4", "四面", "interview", "#a21caf", false, true},
	{"hrbp", "HRBP 面", "interview", "#db2777", false, true},
	{"offer_wait", "Offer 审批中", "normal", "#d97706", false, false},
	{"offer", "已发 Offer", "terminal_success", "#16a34a", false, false},
	{"rejected", "已结束", "terminal_fail", "#6b7280", false, false},
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
		requires, skippable := 0, 0
		if s.requiresOwner {
			requires = 1
		}
		if s.skippable {
			skippable = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO stages (board_id, key, label, kind, color, requires_owner, skippable, position, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			boardID, s.key, s.label, s.kind, s.color, requires, skippable,
			float64((i+1)*1000), nowStr); err != nil {
			return fmt.Errorf("写入种子阶段失败: %w", err)
		}
	}

	// 这是一个人自己用的求职看板，成员表里就一条：本人。
	// 「跟进人」这套字段保留着，将来想拉人一起用不用改数据模型。
	members := []struct{ name, role, color string }{
		{SoloMemberName, "lead", "#2563eb"},
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
		{"某电商", "Go 开发", "官网投递", "在线测评 90 分钟，限时", "online_test", "normal", ids[0]},
		{"某创业公司", "基础架构", "猎头", "HRBP 沟通薪资中", "hrbp", "high", ids[0]},
		{"某外企", "平台工程", "官网投递", "面试官反馈方向不匹配", "rejected", "low", ids[0]},
		{"某游戏公司", "服务端开发", "内推", "一面还没约上时间", "round_1", "normal", ids[0]},
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
