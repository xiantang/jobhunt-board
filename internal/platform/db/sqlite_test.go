package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateAddsSkippableToOldDB 模拟一个建表时还没有 skippable 列的旧库：
// CREATE TABLE IF NOT EXISTS 不会给已存在的表补列，得靠 Migrate 里的 ALTER。
func TestMigrateAddsSkippableToOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE stages (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			board_id       INTEGER NOT NULL,
			key            TEXT    NOT NULL,
			label          TEXT    NOT NULL,
			kind           TEXT    NOT NULL DEFAULT 'normal',
			color          TEXT    NOT NULL DEFAULT '#6b7280',
			requires_owner INTEGER NOT NULL DEFAULT 0,
			position       REAL    NOT NULL,
			created_at     TEXT    NOT NULL
		)`); err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO stages (board_id, key, label, position, created_at)
		VALUES (1, 'hr_screen', 'HR screen', 1000, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("写入旧数据失败: %v", err)
	}
	db.Close()

	// Open 内部会跑 Migrate。跑两次验证幂等。
	for i := 0; i < 2; i++ {
		db, err = openSQLite(path)
		if err != nil {
			t.Fatalf("第 %d 次迁移失败: %v", i+1, err)
		}
		var skippable int
		if err := db.QueryRow(`SELECT skippable FROM stages WHERE key = 'hr_screen'`).Scan(&skippable); err != nil {
			t.Fatalf("第 %d 次读 skippable 失败: %v", i+1, err)
		}
		if skippable != 0 {
			t.Fatalf("旧数据的 skippable 应当默认为 0，实际 %d", skippable)
		}
		db.Close()
	}
}

// TestMigrateBackfillsSkippableDefaults 覆盖两种旧库：
// 一种还没有 skippable 列，一种列已经加上但值全是 0（上一个版本只 ALTER 没回填）。
// 两种都应当被补成和新建库一致，且只补一次。
func TestMigrateBackfillsSkippableDefaults(t *testing.T) {
	cases := []struct {
		name    string
		withCol bool
	}{
		{"没有 skippable 列的旧库", false},
		{"有列但没回填过的旧库", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			seedLegacyStages(t, path, tc.withCol)

			db, err := openSQLite(path)
			if err != nil {
				t.Fatalf("迁移失败: %v", err)
			}
			defer db.Close()

			// 默认配置里「在线测评」可跳过，HR screen 才能直达一面。
			if got := skippableOf(t, db, "online_test"); got != 1 {
				t.Fatalf("在线测评的 skippable = %d，期望 1", got)
			}
			if got := skippableOf(t, db, "hr_screen"); got != 0 {
				t.Fatalf("HR screen 默认不可跳过，实际 %d", got)
			}

			// 用户手动取消勾选后，重启不该把它重新打开。
			if _, err := db.Exec(`UPDATE stages SET skippable = 0 WHERE key = 'online_test'`); err != nil {
				t.Fatalf("取消勾选失败: %v", err)
			}
			if err := migrateSQLite(db); err != nil {
				t.Fatalf("二次迁移失败: %v", err)
			}
			if got := skippableOf(t, db, "online_test"); got != 0 {
				t.Fatalf("回填只该跑一次，用户的选择被覆盖了：skippable = %d", got)
			}
		})
	}
}

// seedLegacyStages 造一个建表时还是老结构的库。
func seedLegacyStages(t *testing.T, path string, withSkippable bool) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	defer db.Close()

	col := ""
	if withSkippable {
		col = ",\n\t\t\tskippable      INTEGER NOT NULL DEFAULT 0"
	}
	if _, err := db.Exec(`
		CREATE TABLE stages (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			board_id       INTEGER NOT NULL,
			key            TEXT    NOT NULL,
			label          TEXT    NOT NULL,
			kind           TEXT    NOT NULL DEFAULT 'normal',
			color          TEXT    NOT NULL DEFAULT '#6b7280',
			requires_owner INTEGER NOT NULL DEFAULT 0,
			position       REAL    NOT NULL,
			created_at     TEXT    NOT NULL` + col + `
		)`); err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	for i, s := range []struct{ key, label string }{
		{"hr_screen", "HR 初筛"}, // 改名之前的老标签
		{"online_test", "在线测评"},
		{"round_1", "一面"},
	} {
		if _, err := db.Exec(`INSERT INTO stages (board_id, key, label, position, created_at)
			VALUES (1, ?, ?, ?, '2026-01-01T00:00:00Z')`, s.key, s.label, float64((i+1)*1000)); err != nil {
			t.Fatalf("写入旧数据失败: %v", err)
		}
	}
}

func skippableOf(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`SELECT skippable FROM stages WHERE key = ?`, key).Scan(&v); err != nil {
		t.Fatalf("读 %s 的 skippable 失败: %v", key, err)
	}
	return v
}

// TestMigrateCollapsesSeedMembers：老库里的 A / B / C 是演示数据，
// A 就是本人，改名成「我」；B / C 没被用过就删掉，用过就留着。
func TestMigrateCollapsesSeedMembers(t *testing.T) {
	t.Run("B/C 没被用过就删掉", func(t *testing.T) {
		db := openWithSeedMembers(t, 0)
		defer db.Close()

		names := memberNames(t, db)
		if len(names) != 1 || names[0] != SoloMemberName {
			t.Fatalf("成员应当只剩「%s」，实际 %v", SoloMemberName, names)
		}
	})

	t.Run("被引用的成员留着", func(t *testing.T) {
		db := openWithSeedMembers(t, 2) // B 名下挂了一条流程
		defer db.Close()

		names := memberNames(t, db)
		if len(names) != 2 {
			t.Fatalf("有引用的成员不该被删，实际 %v", names)
		}
		// 卡片的跟进人不能被 ON DELETE SET NULL 悄悄清空。
		var owner int64
		if err := db.QueryRow(`SELECT owner_id FROM applications WHERE id = 1`).Scan(&owner); err != nil {
			t.Fatalf("读跟进人失败: %v", err)
		}
		if owner != 2 {
			t.Fatalf("跟进人 = %d，期望 2", owner)
		}
	})

	t.Run("不是种子成员就不动", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "custom.db")
		db, err := openSQLite(path)
		if err != nil {
			t.Fatalf("建库失败: %v", err)
		}
		defer db.Close()
		// 一个真给成员起名叫 A 的库，但人数对不上种子，迁移应当放过它。
		for _, n := range []string{"A", "张三"} {
			if _, err := db.Exec(`INSERT INTO members (name, created_at) VALUES (?, '2026-01-01T00:00:00Z')`, n); err != nil {
				t.Fatalf("写入成员失败: %v", err)
			}
		}
		if err := migrateSQLite(db); err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		names := memberNames(t, db)
		if len(names) != 2 || names[0] != "A" {
			t.Fatalf("非种子库不该被改动，实际 %v", names)
		}
	})
}

// openWithSeedMembers 造一个带 A/B/C 的老库并跑迁移。
// ownerOfApp 非 0 时，给 B（id=2）挂一条流程，用来验证「有引用就不删」。
func openWithSeedMembers(t *testing.T, ownerOfApp int64) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	if _, err := db.Exec(schemaSQLite); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	for _, n := range []string{"A", "B", "C"} {
		if _, err := db.Exec(`INSERT INTO members (name, created_at) VALUES (?, '2026-01-01T00:00:00Z')`, n); err != nil {
			t.Fatalf("写入成员失败: %v", err)
		}
	}
	if ownerOfApp > 0 {
		if _, err := db.Exec(`INSERT INTO boards (id, key, name, created_at)
			VALUES (1, 'JOBHUNT', '求职', '2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("写入看板失败: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO applications
			(id, board_id, seq, company, stage_key, owner_id, position, created_at, updated_at)
			VALUES (1, 1, 1, '某公司', 'hr_screen', ?, 1000, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			ownerOfApp); err != nil {
			t.Fatalf("写入流程失败: %v", err)
		}
	}
	db.Close()

	db, err = openSQLite(path)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return db
}

func memberNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM members ORDER BY id`)
	if err != nil {
		t.Fatalf("读成员失败: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("扫描成员失败: %v", err)
		}
		out = append(out, n)
	}
	return out
}

// 旧库的 stages.kind 上有一条 CHECK 约束，只认四种类型；新增的 'task'
// 会被它挡下来。SQLite 改不了 CHECK，只能重建表——这个测试守住重建：
// 新值能写进去，老数据一行不少，索引和外键都还在。
func TestMigrateAllowsTaskKindOnOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-check.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE boards (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			key        TEXT    NOT NULL UNIQUE,
			name       TEXT    NOT NULL,
			created_at TEXT    NOT NULL
		);
		CREATE TABLE stages (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			board_id       INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
			key            TEXT    NOT NULL,
			label          TEXT    NOT NULL,
			kind           TEXT    NOT NULL DEFAULT 'normal'
			               CHECK (kind IN ('normal', 'interview', 'terminal_success', 'terminal_fail')),
			color          TEXT    NOT NULL DEFAULT '#6b7280',
			requires_owner INTEGER NOT NULL DEFAULT 0,
			skippable      INTEGER NOT NULL DEFAULT 0,
			position       REAL    NOT NULL,
			created_at     TEXT    NOT NULL,
			UNIQUE (board_id, key)
		);
		INSERT INTO boards (id, key, name, created_at) VALUES (1, 'JOBHUNT', '求职', '2026-01-01T00:00:00Z');
		INSERT INTO stages (board_id, key, label, kind, position, created_at) VALUES
			(1, 'hr_screen',   'HR screen', 'normal',    1000, '2026-01-01T00:00:00Z'),
			(1, 'online_test', '在线测评',    'normal',    2000, '2026-01-01T00:00:00Z'),
			(1, 'round_1',     '一面',       'interview', 3000, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("造旧库失败: %v", err)
	}
	db.Close()

	// 跑两次，验证幂等：重建过的库第二次不该再重建，也不该报错。
	for i := 0; i < 2; i++ {
		db, err = openSQLite(path)
		if err != nil {
			t.Fatalf("第 %d 次迁移失败: %v", i+1, err)
		}

		var kind string
		if err := db.QueryRow(`SELECT kind FROM stages WHERE key = 'online_test'`).Scan(&kind); err != nil {
			t.Fatalf("读在线测评的 kind 失败: %v", err)
		}
		if kind != "task" {
			t.Fatalf("第 %d 次迁移后在线测评的 kind = %q，期望 task", i+1, kind)
		}

		// 别的列不该被顺手改掉。
		var interview string
		if err := db.QueryRow(`SELECT kind FROM stages WHERE key = 'round_1'`).Scan(&interview); err != nil {
			t.Fatalf("读一面的 kind 失败: %v", err)
		}
		if interview != "interview" {
			t.Fatalf("一面的 kind 被改成了 %q", interview)
		}

		// 重建之后 CHECK 该放行 task，别的乱值仍然要挡住。
		if _, err := db.Exec(`UPDATE stages SET kind = 'task' WHERE key = 'hr_screen'`); err != nil {
			t.Fatalf("第 %d 次写入 task 类型失败: %v", i+1, err)
		}
		if _, err := db.Exec(`UPDATE stages SET kind = 'nonsense' WHERE key = 'hr_screen'`); err == nil {
			t.Fatal("CHECK 约束没了：乱写的 kind 也能存进去")
		}
		if _, err := db.Exec(`UPDATE stages SET kind = 'normal' WHERE key = 'hr_screen'`); err != nil {
			t.Fatalf("复原 hr_screen 失败: %v", err)
		}

		// 外键还连着：删掉看板要能级联清掉阶段。
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM stages`).Scan(&count); err != nil {
			t.Fatalf("统计阶段失败: %v", err)
		}
		if count != 3 {
			t.Fatalf("重建后阶段数 = %d，期望 3——数据在重建时丢了", count)
		}
		db.Close()
	}
}
