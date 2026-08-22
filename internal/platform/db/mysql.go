// MySQL 是线上用的库：托管实例，Pod 重建、扩副本都不影响数据。
package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

//go:embed schema_mysql.sql
var schemaMySQL string

// openMySQL 连上托管实例并建表。
//
// dsn 是 go-sql-driver 的格式：user:pass@tcp(host:3306)/dbname。
// 剩下的参数（字符集、时区、超时）这里统一钉死，不指望运维在 DSN 里写对——
// 尤其是 charset：默认的 utf8mb3 存不下 emoji，公司名和备注里真的会有。
func openMySQL(dsn string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析 MySQL DSN 失败: %w", err)
	}
	if cfg.DBName == "" {
		return nil, fmt.Errorf("MySQL DSN 里没有库名")
	}
	cfg.Collation = "utf8mb4_general_ci"
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	// ANSI_QUOTES 让 "key" 这样的双引号被当成标识符而不是字符串。
	// stages / boards 上都有一列叫 key，它在 MySQL 里是保留字，不加引号跑不了；
	// 而 Go 的原始字符串里塞不进反引号，SQL 就得拆成一段段拼接。
	// 打开这个开关，双引号在两边是同一个意思，业务层只用维护一份 SQL。
	// CONCAT 是往默认 sql_mode 后面追加，不是覆盖——STRICT_TRANS_TABLES 那些得留着。
	cfg.Params["sql_mode"] = "CONCAT(@@sql_mode, ',ANSI_QUOTES')"
	// 时间列存的是 RFC3339 字符串，不走驱动的时间解析——parseTime 保持关闭，
	// 这样 SQLite 和 MySQL 读出来的是同一个东西，业务层不用分两套。
	cfg.ParseTime = false
	// ClientFoundRows 让 RowsAffected 返回「匹配到几行」而不是「改动了几行」，
	// 和 SQLite 一致。否则用同样的值 UPDATE 一行，MySQL 报 0，
	// 调用方会以为这行不存在（calendar/store.go 的 save 就靠这个判断该不该插入）。
	cfg.ClientFoundRows = true
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	// 云上的 MySQL 和中间的 NAT 都会悄悄掐掉闲置连接，
	// 主动比它们先回收，免得拿到一条已经死掉的连接。
	db.SetConnMaxLifetime(3 * time.Minute)
	db.SetConnMaxIdleTime(time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	if err := migrateMySQL(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrateMySQL 执行幂等的建表语句。
//
// 没有 SQLite 那边的历史补丁：那些是给早期版本留下的库打的，
// MySQL 是从这一版才开始用的，建出来就是最终形态。
func migrateMySQL(db *sql.DB) error {
	for _, stmt := range splitStatements(schemaMySQL) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("初始化表结构失败（%s）: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// splitStatements 把 schema 按分号拆成单条语句。
//
// 不开驱动的 MultiStatements：那个开关对整个连接生效，
// 为了建表一次方便，把之后每一条业务查询都变成可以塞多条语句的通道，不划算。
func splitStatements(schema string) []string {
	var out []string
	for _, s := range strings.Split(schema, ";") {
		if strings.TrimSpace(stripComments(s)) != "" {
			out = append(out, s)
		}
	}
	return out
}

// stripComments 去掉 -- 开头的注释行，只用来判断这一段是不是空语句。
func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			b.WriteString(line)
		}
	}
	return b.String()
}

// firstLine 从建表语句里摘出一行放进错误信息，方便一眼看出是哪张表挂了。
func firstLine(stmt string) string {
	for _, line := range strings.Split(strings.TrimSpace(stmt), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
			return line
		}
	}
	return "?"
}
