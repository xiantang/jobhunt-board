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
