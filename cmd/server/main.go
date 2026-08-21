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

	"github.com/gin-gonic/gin"

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

	srv := &http.Server{Addr: *addr, Handler: server.New(db, logger)}

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
