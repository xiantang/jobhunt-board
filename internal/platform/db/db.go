// Package db 负责数据库连接、表结构迁移与首次启动的种子数据。
//
// 本地开发用 SQLite（一个文件，零依赖），线上用 MySQL（托管实例，多副本可共享）。
// 两边的业务 SQL 完全一样：时间统一按 RFC3339 存字符串，布尔存 0/1，
// 只有建表语句和「打开连接」这一步分方言。Open 按配置挑一条路走。
package db

import (
	"database/sql"
	"fmt"
	"time"
)

// Driver 是数据库方言。
type Driver string

const (
	SQLite Driver = "sqlite"
	MySQL  Driver = "mysql"
)

// Config 描述连哪个库。SQLite 用 DSN 当文件路径，MySQL 用它当 go-sql-driver 的 DSN。
type Config struct {
	Driver Driver
	DSN    string
}

// Open 按方言打开数据库并完成迁移。
func Open(cfg Config) (*sql.DB, error) {
	switch cfg.Driver {
	case SQLite:
		return openSQLite(cfg.DSN)
	case MySQL:
		return openMySQL(cfg.DSN)
	default:
		return nil, fmt.Errorf("未知的数据库类型 %q", cfg.Driver)
	}
}

// SoloMemberName 是默认成员名——这个看板是一个人在用，操作日志和卡片头像都显示它。
const SoloMemberName = "我"

// defaultStages 是新看板的默认面试流程。用户可以在页面上随意增删改序，
// 这里只提供一个开箱即用的起点。
var defaultStages = []struct {
	key, label, kind, color string
	requiresOwner           bool
	skippable               bool
}{
	{"hr_screen", "HR screen", "normal", "#64748b", false, false},
	// 不是每家公司都有在线测评，默认可跳过：HR screen 能直达一面。
	// 测评不约时间，只有一个截止时刻和一条链接——所以是任务阶段而不是普通阶段。
	{"online_test", "在线测评", "task", "#0891b2", false, true},
	{"round_1", "一面", "interview", "#2563eb", true, false},
	{"round_2", "二面", "interview", "#4f46e5", false, false},
	{"round_3", "三面", "interview", "#7c3aed", false, false},
	// 一面、二面、三面面完都可能卡在等通知，所以它是「等待回复」类型：
	// 摆在三面旁边只是个位置，任意阶段都能进出它（workflow.Kind.Unordered）。
	// 可跳过，否则它会挡住三面直接推进到四面。
	{"waiting_reply", "等待回复中", "waiting", "#ca8a04", false, true},
	{"round_4", "四面", "interview", "#a21caf", false, true},
	{"hrbp", "HRBP 面", "interview", "#db2777", false, true},
	{"offer_wait", "Offer 审批中", "normal", "#d97706", false, false},
	{"offer", "已发 Offer", "terminal_success", "#16a34a", false, false},
	{"rejected", "已结束", "terminal_fail", "#6b7280", false, false},
}

// Seed 仅在库为空时写入演示数据：一个看板、十一个阶段、三名成员、五条面试流程。
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

	res, err := tx.Exec(`INSERT INTO boards ("key", name, description, created_at) VALUES (?, ?, ?, ?)`,
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
			INSERT INTO stages (board_id, "key", label, kind, color, requires_owner, skippable, position, created_at)
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
		{"某大厂", "后端研发工程师", "内推", "内推码已用，简历已过初筛", "round_2", "high", ids[0]},
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

// waitingStage 是「等待回复中」那一列在 defaultStages 里的定义。
// EnsureWaitingStage 给老看板补列时用的是同一份，两处不会写岔；
// 按 key 找而不是按下标，往 defaultStages 中间插一列不会把它指飞。
var waitingStage = func() struct {
	key, label, kind, color string
	requiresOwner           bool
	skippable               bool
} {
	for _, s := range defaultStages {
		if s.kind == "waiting" {
			return s
		}
	}
	panic("defaultStages 里没有等待回复列")
}()

// EnsureWaitingStage 给已经在用的看板补上「等待回复中」这一列。
//
// Seed 只在空库上跑，往 defaultStages 里加一列只对全新的库有效 —— 而这一列
// 恰恰是给正在面的人用的，不能只让新用户享受。所以它每次启动都跑一遍：
// 已经有等待回复类型的列就跳过，两个方言共用这一份逻辑。
//
// 落位规则和 defaultStages 一致：插在三面后面。看板被改造过、找不到三面时
// 退到第一个终态之前——那是「再加一列」的通用直觉，和 stage.Create 一致。
func EnsureWaitingStage(conn *sql.DB) error {
	rows, err := conn.Query(`SELECT id FROM boards ORDER BY id`)
	if err != nil {
		return fmt.Errorf("读取看板失败: %w", err)
	}
	defer rows.Close()

	ids := make([]int64, 0, 4)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("读取看板失败: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取看板失败: %w", err)
	}

	for _, id := range ids {
		if err := ensureWaitingStageOn(conn, id); err != nil {
			return err
		}
	}
	return nil
}

func ensureWaitingStageOn(conn *sql.DB, boardID int64) error {
	type stage struct {
		key, kind string
		position  float64
	}

	rows, err := conn.Query(
		`SELECT "key", kind, position FROM stages WHERE board_id = ? ORDER BY position, id`, boardID)
	if err != nil {
		return fmt.Errorf("读取看板 %d 的阶段失败: %w", boardID, err)
	}
	defer rows.Close()

	stages := make([]stage, 0, 12)
	for rows.Next() {
		var st stage
		if err := rows.Scan(&st.key, &st.kind, &st.position); err != nil {
			return fmt.Errorf("读取看板 %d 的阶段失败: %w", boardID, err)
		}
		stages = append(stages, st)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取看板 %d 的阶段失败: %w", boardID, err)
	}
	if len(stages) == 0 { // 空看板交给 Seed，这里不插一列孤零零的
		return nil
	}
	for _, st := range stages {
		// 已经有一列在干这件事了就别再插一列——不管它是我们插的，
		// 还是用户自己建的、改了名的。
		if st.key == waitingStage.key || st.kind == "waiting" {
			return nil
		}
	}

	// 落点：三面后面；没有三面就退到第一个终态之前；连终态都没有就追加到末尾。
	anchor := -1
	for i, st := range stages {
		if st.key == "round_3" {
			anchor = i
			break
		}
	}
	var before, after float64
	switch {
	case anchor >= 0:
		before = stages[anchor].position
		after = before + 2000
		if anchor+1 < len(stages) {
			after = stages[anchor+1].position
		}
	default:
		first := len(stages)
		for i, st := range stages {
			if st.kind == "terminal_success" || st.kind == "terminal_fail" {
				first = i
				break
			}
		}
		after = stages[len(stages)-1].position + 2000
		if first < len(stages) {
			after = stages[first].position
		}
		before = after - 2000
		if first > 0 {
			before = stages[first-1].position
		}
	}

	_, err = conn.Exec(`
		INSERT INTO stages (board_id, "key", label, kind, color, requires_owner, skippable, position, created_at)
		VALUES (?, ?, ?, ?, ?, 0, 1, ?, ?)`,
		boardID, waitingStage.key, waitingStage.label, waitingStage.kind, waitingStage.color,
		(before+after)/2, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("给看板 %d 补「%s」列失败: %w", boardID, waitingStage.label, err)
	}
	return nil
}
