// SQLite 是本地开发用的库：一个文件，跑测试和本机起服务都不用装东西。
// 这里的迁移全是历史包袱——线上 MySQL 是全新库，不需要它们。
package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema_sqlite.sql
var schemaSQLite string

// openSQLite 打开（必要时创建）数据库文件，设置 PRAGMA 并执行迁移。
func openSQLite(path string) (*sql.DB, error) {
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
	if err := migrateSQLite(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrateSQLite 执行幂等的建表语句，补上建表之后新增的列，再跑一次性的数据修补。
func migrateSQLite(db *sql.DB) error {
	if _, err := db.Exec(schemaSQLite); err != nil {
		return fmt.Errorf("初始化表结构失败: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS 不会给已存在的表加列，旧库要单独补。
	if _, err := addColumn(db, "stages", "skippable", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := addColumn(db, "interview_rounds", "google_event_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := addColumn(db, "interview_rounds", "kind", "TEXT NOT NULL DEFAULT 'interview'"); err != nil {
		return err
	}
	if err := once(db, "stage_kind_task", allowTaskKind); err != nil {
		return err
	}
	if err := once(db, "skippable_defaults", backfillSkippable); err != nil {
		return err
	}
	return once(db, "solo_member", collapseSeedMembers)
}

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

// allowTaskKind 让旧库的 stages 接受新的 'task' 类型，并把「在线测评」转过去。
//
// kind 上有一条 CHECK 约束，而 SQLite 改不了 CHECK——只能按官方推荐的
// 12 步流程重建表。整个过程包在事务里，中途失败会整体回滚，
// 不会留下一个只剩半张表的库。
//
// 期间必须关掉外键：applications 通过 (board_id, key) 指向 stages，
// 开着外键 DROP 掉父表会被拒绝。PRAGMA foreign_keys 又不能在事务里改，
// 所以顺序是「关外键 → 事务重建 → 开外键」。
func allowTaskKind(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'stages'`).Scan(&ddl); err != nil {
		return fmt.Errorf("读取 stages 表结构失败: %w", err)
	}
	if strings.Contains(ddl, "'task'") { // 新建的库本来就带上了，不用重建
		return backfillTaskStages(db)
	}

	if _, err := db.Exec(`PRAGMA foreign_keys = off`); err != nil {
		return fmt.Errorf("重建 stages 前关闭外键失败: %w", err)
	}
	defer db.Exec(`PRAGMA foreign_keys = on`)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("重建 stages 开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		CREATE TABLE stages_rebuild (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			board_id       INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
			key            TEXT    NOT NULL,
			label          TEXT    NOT NULL,
			kind           TEXT    NOT NULL DEFAULT 'normal'
			               CHECK (kind IN ('normal', 'interview', 'task', 'terminal_success', 'terminal_fail')),
			color          TEXT    NOT NULL DEFAULT '#6b7280',
			requires_owner INTEGER NOT NULL DEFAULT 0,
			skippable      INTEGER NOT NULL DEFAULT 0,
			position       REAL    NOT NULL,
			created_at     TEXT    NOT NULL,
			UNIQUE (board_id, key)
		)`); err != nil {
		return fmt.Errorf("建 stages 新表失败: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO stages_rebuild (id, board_id, key, label, kind, color, requires_owner, skippable, position, created_at)
		SELECT id, board_id, key, label, kind, color, requires_owner, skippable, position, created_at FROM stages`); err != nil {
		return fmt.Errorf("迁移 stages 数据失败: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE stages`); err != nil {
		return fmt.Errorf("删除 stages 旧表失败: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE stages_rebuild RENAME TO stages`); err != nil {
		return fmt.Errorf("重命名 stages 新表失败: %w", err)
	}
	// 索引跟着旧表一起没了，得重新建。
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_stages_board ON stages (board_id, position)`); err != nil {
		return fmt.Errorf("重建 stages 索引失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("重建 stages 提交失败: %w", err)
	}
	return backfillTaskStages(db)
}

// backfillTaskStages 把旧库里的「在线测评」转成任务阶段，并给它下面已有的
// 记录打上 task 标记。只动还是 'normal' 的那些——用户要是已经把这一列
// 改成了别的类型，那是他的选择，不该被迁移覆盖回去。
func backfillTaskStages(db *sql.DB) error {
	if _, err := db.Exec(
		`UPDATE stages SET kind = 'task' WHERE key = 'online_test' AND kind = 'normal'`); err != nil {
		return fmt.Errorf("把在线测评转成任务阶段失败: %w", err)
	}
	if _, err := db.Exec(`
		UPDATE interview_rounds SET kind = 'task'
		WHERE stage_key IN (SELECT key FROM stages WHERE kind = 'task')`); err != nil {
		return fmt.Errorf("回填任务记录的 kind 失败: %w", err)
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
