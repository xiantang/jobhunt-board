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
	"interview/internal/platform/sqlitedb"
	"interview/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	dbPath := flag.String("db", "data/app.db", "SQLite 数据文件路径")
	mode := flag.String("mode", gin.ReleaseMode, "gin 运行模式：debug / release")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	gin.SetMode(*mode)

	db, err := sqlitedb.Open(*dbPath)
	if err != nil {
		logger.Error("启动失败", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := sqlitedb.Seed(db); err != nil {
		logger.Error("写入种子数据失败", "error", err)
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

	srv := &http.Server{Addr: *addr, Handler: server.New(db, logger, model, google)}

	go func() {
		logger.Info("服务已启动", "addr", *addr, "db", *dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务异常退出", "error", err)
			os.Exit(1)
		}
	}()

	// 优雅关闭，保证 SQLite 写入落盘。
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
