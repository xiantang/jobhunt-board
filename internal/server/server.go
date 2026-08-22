// Package server 负责装配依赖与 gin 路由。
package server

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"interview/internal/ai"
	"interview/internal/api"
	"interview/internal/application"
	"interview/internal/board"
	"interview/internal/ingest"
	"interview/internal/member"
	"interview/internal/platform/ginx"
	"interview/internal/stage"
	"interview/internal/web"
)

// New 构建完整的 gin 引擎：中间件 → 页面路由 → /api 分组 → 静态资源。
// model 为 nil 表示没配 OpenAI key，AI 录入功能关闭，其余功能不受影响。
func New(db *sql.DB, logger *slog.Logger, model ai.Completer) *gin.Engine {
	members := member.NewService(db)
	boards := board.NewService(db)
	stages := stage.NewService(db)
	applications := application.NewService(db, members, boards, stages)
	ingestion := ingest.NewService(model, stages, applications)

	apiHandler := api.New(members, boards, stages, applications, ingestion)
	pages := web.NewHandler(apiHandler, members)

	r := gin.New()
	r.Use(gin.Recovery(), ginx.ErrorHandler(logger), ginx.CurrentMember(members))
	r.SetHTMLTemplate(web.Templates())
	r.StaticFS("/static", web.StaticFS())

	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	pages.Register(r)
	apiHandler.Register(r.Group("/api"))

	return r
}
