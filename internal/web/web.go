// Package web 是页面层：服务端渲染看板首屏，并把静态资源打进二进制。
package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"

	"interview/internal/api"
	"interview/internal/board"
	"interview/internal/member"
	"interview/internal/platform/ginx"
	"interview/internal/workflow"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Templates 解析内嵌模板，供 gin 的 SetHTMLTemplate 使用。
func Templates() *template.Template {
	return template.Must(template.New("").Funcs(template.FuncMap{
		"deref": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		// jsonify 把看板数据原样交给前端，board.js 据此渲染任意数量的阶段列。
		"jsonify": func(v any) template.JS {
			raw, err := json.Marshal(v)
			if err != nil {
				return template.JS("null")
			}
			return template.JS(raw)
		},
	}).ParseFS(templateFS, "templates/*.html"))
}

// StaticFS 返回可挂到 /static 的文件系统。
func StaticFS() http.FileSystem {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return http.FS(sub)
}

// Handler 渲染页面。
type Handler struct {
	api     *api.Handler
	members *member.Service
}

// NewHandler 构造页面 handler。
func NewHandler(apiHandler *api.Handler, members *member.Service) *Handler {
	return &Handler{api: apiHandler, members: members}
}

// Register 挂载页面路由。
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/", h.Home)
	r.GET("/boards/:key", h.Board)
	r.GET("/boards/:key/calendar", h.Calendar)
}

// Home 跳转到默认看板。
func (h *Handler) Home(c *gin.Context) {
	c.Redirect(http.StatusFound, "/boards/"+board.DefaultKey)
}

// Calendar 渲染日程页：看板的面试 + Google 日历的会议，默认一周。
// 和看板页一样服务端渲染首屏，翻周之后交给 JS 重绘。
func (h *Handler) Calendar(c *gin.Context) {
	b, err := h.api.Boards.GetByKey(c.Request.Context(), c.Param("key"))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	agenda, err := h.api.Agenda(c)
	if err != nil {
		ginx.Fail(c, err)
		return
	}

	c.HTML(http.StatusOK, "calendar.html", gin.H{
		"Board":  b,
		"Agenda": agenda,
		// notice 是 OAuth 回调跳回来时带的结果，页面上给一句提示。
		"Notice": c.Query("google"),
	})
}

// kindOption 是阶段类型下拉的一项。
type kindOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Board 渲染看板首屏（含筛选结果），后续交互走 /api。
func (h *Handler) Board(c *gin.Context) {
	filter, err := api.ParseFilter(c)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	view, err := h.api.Board(c, c.Param("key"), filter)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	members, err := h.members.List(c.Request.Context())
	if err != nil {
		ginx.Fail(c, err)
		return
	}

	kinds := make([]kindOption, 0, 4)
	for _, k := range workflow.Kinds() {
		kinds = append(kinds, kindOption{Value: string(k), Label: workflow.KindLabel(k)})
	}

	c.HTML(http.StatusOK, "board.html", gin.H{
		"View":            view,
		"Members":         members,
		"Kinds":           kinds,
		"CurrentMemberID": ginx.ActorID(c),
		"AIEnabled":       h.api.AIEnabled(),
		"CalendarEnabled": h.api.CalendarEnabled(),
		"FilterStage":     c.DefaultQuery("stage", "all"),
		"FilterOwner":     c.DefaultQuery("owner", "all"),
		"FilterUpcoming":  c.Query("upcoming") == "1",
	})
}
