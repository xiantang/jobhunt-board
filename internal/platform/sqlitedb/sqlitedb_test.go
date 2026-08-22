package sqlitedb

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
		db, err = Open(path)
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

			db, err := Open(path)
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
			if err := Migrate(db); err != nil {
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
