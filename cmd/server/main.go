// 面试流程看板：单一 Go 服务，同时提供看板页面与 JSON 接口。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// 时区库编进二进制。代码里大量用 time.Local（日程按本地时间排布），
	// 而生产镜像是 distroless，里面没有 /usr/share/zoneinfo ——
	// 不 import 它的话容器里 TZ=Asia/Shanghai 查不到东西，会静默退回 UTC，
	// 症状是日程整体偏 8 小时。本地开发有系统时区，察觉不到这个坑。
	_ "time/tzdata"

	"github.com/gin-gonic/gin"

	"interview/internal/ai"
	"interview/internal/gcal"
	"interview/internal/platform/db"
	"interview/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	dbPath := flag.String("db", "data/app.db", "SQLite 数据文件路径（未设置 MYSQL_DSN 时生效）")
	mode := flag.String("mode", gin.ReleaseMode, "gin 运行模式：debug / release")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	gin.SetMode(*mode)

	// 选库的规则只有一条：配了 MYSQL_DSN 就连 MySQL，没配就用本地 SQLite 文件。
	// 本地开发不用装数据库，线上把 DSN 塞进 Secret 就切过去了，代码不用动。
	cfg := db.Config{Driver: db.SQLite, DSN: *dbPath}
	if dsn := os.Getenv("MYSQL_DSN"); dsn != "" {
		cfg = db.Config{Driver: db.MySQL, DSN: dsn}
	}

	conn, err := db.Open(cfg)
	if err != nil {
		logger.Error("启动失败", "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := db.Seed(conn); err != nil {
		logger.Error("写入种子数据失败", "error", err)
		os.Exit(1)
	}
	// 「等待回复中」是后来加的默认列。Seed 只管空库，已经在用的看板得单独补，
	// 否则新增的默认阶段永远到不了真正需要它的人手里。幂等，每次启动跑一遍。
	if err := db.EnsureWaitingStage(conn); err != nil {
		logger.Error("补「等待回复中」列失败", "error", err)
		os.Exit(1)
	}

	// AI 录入靠环境变量开关：没配 OPENAI_API_KEY 就只是少一个入口，服务照常起。
	model := ai.New(ai.ConfigFromEnv())
	if model.Available() {
		logger.Info("AI 录入已启用", "model", model.Model())
	} else {
		logger.Info("AI 录入未启用：未设置 OPENAI_API_KEY")
	}

	google := gcal.New(gcal.ConfigFromEnv())
	if google.Available() {
		logger.Info("Google 日历集成已启用", "redirect_url", google.RedirectURL())
	} else {
		logger.Info("Google 日历集成未启用：未设置 GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET")
	}

	srv := &http.Server{Addr: *addr, Handler: server.New(conn, logger, model, google)}

	go func() {
		logger.Info("服务已启动", "addr", *addr, "db", string(cfg.Driver))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务异常退出", "error", err)
			os.Exit(1)
		}
	}()

	// 优雅关闭：SQLite 要等写入落盘，MySQL 要把在途请求做完再断连接。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("关闭超时", "error", err)
	}
	logger.Info("服务已停止")
}
