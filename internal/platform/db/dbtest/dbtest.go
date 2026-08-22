// Package dbtest 给各层的测试开一个空库。
//
// 默认是 SQLite 临时文件，测试不依赖任何外部服务。
// 设了 TEST_MYSQL_DSN 就改连那个 MySQL 实例——同一套测试因此能在两个方言上
// 各跑一遍，线上和本地跑的是不是同一份 SQL，这里说了算：
//
//	TEST_MYSQL_DSN='root:root@tcp(127.0.0.1:13306)/jobhunt' go test ./...
package dbtest

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-sql-driver/mysql"

	"interview/internal/platform/db"
)

// New 返回一个刚建好表、还没有任何数据的库，测试结束自动关闭。
func New(t *testing.T) *sql.DB {
	t.Helper()

	cfg := db.Config{Driver: db.SQLite, DSN: filepath.Join(t.TempDir(), "test.db")}
	if dsn := os.Getenv("TEST_MYSQL_DSN"); dsn != "" {
		cfg = db.Config{Driver: db.MySQL, DSN: freshDatabase(t, dsn)}
	}

	conn, err := db.Open(cfg)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// freshDatabase 在同一个实例上建一个随机名字的空库，返回指向它的 DSN，
// 用完即删。
//
// 不是清空 DSN 里那个库：go test 会把多个包的测试并行跑起来，
// 它们连的是同一个实例，谁清表都会把别的包正在用的表删掉。
// 一个测试一个库，隔离就和 SQLite 那边的临时文件一样干净。
func freshDatabase(t *testing.T, dsn string) string {
	t.Helper()

	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("解析 TEST_MYSQL_DSN 失败: %v", err)
	}

	admin := *cfg
	admin.DBName = "" // 先连到实例本身，此刻要建的库还不存在
	conn, err := sql.Open("mysql", admin.FormatDSN())
	if err != nil {
		t.Fatalf("连接测试用 MySQL 失败: %v", err)
	}
	defer conn.Close()

	name := fmt.Sprintf("dbtest_%d", rand.Uint64())
	if _, err := conn.Exec("CREATE DATABASE `" + name + "` DEFAULT CHARACTER SET utf8mb4"); err != nil {
		t.Fatalf("创建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		// 用一条新连接删：此时测试自己那个连接池已经关了。
		c, err := sql.Open("mysql", admin.FormatDSN())
		if err != nil {
			t.Logf("清理测试库 %s 失败: %v", name, err)
			return
		}
		defer c.Close()
		if _, err := c.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
			t.Logf("清理测试库 %s 失败: %v", name, err)
		}
	})

	cfg.DBName = name
	return cfg.FormatDSN()
}
