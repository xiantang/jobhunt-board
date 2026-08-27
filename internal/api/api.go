// Package api 是 JSON 接口层：负责入参绑定、调用 service、组织响应。
// 业务规则不写在这里，错误统一交给 ginx.ErrorHandler 渲染。
package api

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"interview/internal/application"
	"interview/internal/board"
	"interview/internal/calendar"
	"interview/internal/ingest"
	"interview/internal/member"
	"interview/internal/platform/apperr"
	"interview/internal/platform/ginx"
	"interview/internal/stage"
	"interview/internal/workflow"
)

// Handler 持有各业务服务。Ingest 为 nil 表示没配 OpenAI key，AI 录入整体关闭。
type Handler struct {
	Members      *member.Service
	Boards       *board.Service
	Stages       *stage.Service
	Applications *application.Service
	Ingest       *ingest.Service
	Calendar     *calendar.Service
}

// New 构造 API handler。
func New(members *member.Service, boards *board.Service, stages *stage.Service,
	apps *application.Service, ai *ingest.Service, cal *calendar.Service) *Handler {
	return &Handler{
		Members: members, Boards: boards, Stages: stages,
		Applications: apps, Ingest: ai, Calendar: cal,
	}
}

// CalendarEnabled 表示配了 Google OAuth 客户端，页面上才显示日程入口。
func (h *Handler) CalendarEnabled() bool { return h.Calendar != nil && h.Calendar.Enabled() }

// AIEnabled 表示 AI 录入可用，页面据此决定要不要显示入口。
func (h *Handler) AIEnabled() bool { return h.Ingest != nil && h.Ingest.Available() }

// Register 把接口挂到 /api 分组。
func (h *Handler) Register(r *gin.RouterGroup) {
	r.GET("/boards/:key", h.GetBoardInfo)
	r.GET("/boards/:key/board", h.GetBoard)
	r.POST("/boards/:key/applications", h.CreateApplication)
	r.GET("/boards/:key/stages", h.ListStages)
	r.POST("/boards/:key/stages", h.CreateStage)
	r.POST("/boards/:key/ingest/parse", h.ParseIngest)
	r.POST("/boards/:key/ingest/confirm", h.ConfirmIngest)

	r.PATCH("/stages/:id", h.UpdateStage)
	r.PATCH("/stages/:id/position", h.ReorderStage)
	r.DELETE("/stages/:id", h.DeleteStage)

	r.GET("/applications/:id", h.GetApplication)
	r.PATCH("/applications/:id", h.UpdateApplication)
	r.PATCH("/applications/:id/stage", h.MoveApplication)
	r.POST("/applications/:id/rounds", h.ScheduleRound)
	r.DELETE("/applications/:id", h.DeleteApplication)

	r.PATCH("/rounds/:id", h.UpdateRound)
	r.DELETE("/rounds/:id", h.DeleteRound)

	r.GET("/agenda", h.GetAgenda)
	r.GET("/google/status", h.GoogleStatus)
	r.POST("/google/disconnect", h.GoogleDisconnect)
	r.POST("/rounds/:id/sync", h.SyncRound)

	r.GET("/members", h.ListMembers)
	r.POST("/members", h.CreateMember)
	r.POST("/session/member", h.SwitchMember)
}

// BoardColumn 是看板的一列，等价于一个阶段。
type BoardColumn struct {
	ID            int64                     `json:"id"`
	Key           string                    `json:"key"`
	Label         string                    `json:"label"`
	Kind          workflow.Kind             `json:"kind"`
	KindLabel     string                    `json:"kind_label"`
	Color         string                    `json:"color"`
	RequiresOwner bool                      `json:"requires_owner"`
	Skippable     bool                      `json:"skippable"` // 可以被跨过去
	IsEntry       bool                      `json:"is_entry"`  // 第一列，新流程从这里进
	Terminal      bool                      `json:"terminal"`
	Count         int                       `json:"count"`
	Applications  []application.Application `json:"applications"`
}

// BoardView 是看板页面/接口共用的视图数据。
type BoardView struct {
	Board   board.Board   `json:"board"`
	Summary board.Summary `json:"summary"`
	Columns []BoardColumn `json:"columns"`
}

// Board 组装某看板的数据：摘要始终统计全量，列内容受筛选影响。
// 列由 stages 表决定，所以用户改完阶段配置，界面立刻跟着变。
func (h *Handler) Board(c *gin.Context, key string, f application.Filter) (BoardView, error) {
	ctx := c.Request.Context()

	b, err := h.Boards.GetByKey(ctx, key)
	if err != nil {
		return BoardView{}, err
	}
	summary, err := h.Boards.SummaryOf(ctx, b.ID)
	if err != nil {
		return BoardView{}, err
	}
	stages, err := h.Stages.List(ctx, b.ID)
	if err != nil {
		return BoardView{}, err
	}
	items, err := h.Applications.List(ctx, b.ID, f)
	if err != nil {
		return BoardView{}, err
	}

	columns := make([]BoardColumn, 0, len(stages))
	for i, st := range stages {
		col := BoardColumn{
			ID:            st.ID,
			Key:           st.Key,
			Label:         st.Label,
			Kind:          st.Kind,
			KindLabel:     st.KindLabel,
			Color:         st.Color,
			RequiresOwner: st.RequiresOwner,
			Skippable:     st.Skippable,
			IsEntry:       i == 0,
			Terminal:      st.Kind.Terminal(),
			Applications:  []application.Application{},
		}
		for _, a := range items {
			if a.StageKey == st.Key {
				col.Applications = append(col.Applications, a)
			}
		}
		col.Count = len(col.Applications)
		columns = append(columns, col)
	}
	return BoardView{Board: b, Summary: summary, Columns: columns}, nil
}

// ParseFilter 从 query 解析筛选条件：
// ?stage=round_1&owner=2&upcoming=1（owner=none 表示未指定跟进人）。
func ParseFilter(c *gin.Context) (application.Filter, error) {
	var f application.Filter

	if raw := c.Query("stage"); raw != "" && raw != "all" {
		key := raw
		f.StageKey = &key
	}
	if raw := c.Query("owner"); raw != "" && raw != "all" {
		if raw == "none" {
			var unassigned int64
			f.OwnerID = &unassigned
		} else {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return f, apperr.Invalid("owner", "跟进人筛选参数不合法")
			}
			f.OwnerID = &id
		}
	}
	f.Upcoming = c.Query("upcoming") == "1"
	return f, nil
}

// GetBoardInfo 返回看板信息与进展摘要。
func (h *Handler) GetBoardInfo(c *gin.Context) {
	b, err := h.Boards.GetByKey(c.Request.Context(), c.Param("key"))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	summary, err := h.Boards.SummaryOf(c.Request.Context(), b.ID)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.OK(c, gin.H{"board": b, "summary": summary})
}

// GetBoard 返回整块看板数据，前端每次变更后据此刷新。
func (h *Handler) GetBoard(c *gin.Context) {
	f, err := ParseFilter(c)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	view, err := h.Board(c, c.Param("key"), f)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.OK(c, view)
}

// ---------- 阶段配置 ----------

// ListStages 返回看板的阶段配置，供配置面板渲染。
func (h *Handler) ListStages(c *gin.Context) {
	b, err := h.Boards.GetByKey(c.Request.Context(), c.Param("key"))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	stages, err := h.Stages.List(c.Request.Context(), b.ID)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.OK(c, gin.H{"stages": stages, "kinds": kindOptions()})
}

// CreateStageReq 是新增阶段的请求体。
type CreateStageReq struct {
	Label         string `json:"label"          binding:"required,max=20"`
	Kind          string `json:"kind"           binding:"omitempty,oneof=normal interview terminal_success terminal_fail"`
	Color         string `json:"color"          binding:"omitempty,max=20"`
	RequiresOwner bool   `json:"requires_owner"`
	Skippable     bool   `json:"skippable"`
	Index         *int   `json:"index"` // 插到第几列，缺省插到终态阶段之前
}

// CreateStage 给看板加一个阶段。
func (h *Handler) CreateStage(c *gin.Context) {
	var req CreateStageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	b, err := h.Boards.GetByKey(c.Request.Context(), c.Param("key"))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	index := -1
	if req.Index != nil {
		index = *req.Index
	}
	created, err := h.Stages.Create(c.Request.Context(), b.ID, stage.CreateInput{
		Label:         req.Label,
		Kind:          req.Kind,
		Color:         req.Color,
		RequiresOwner: req.RequiresOwner,
		Skippable:     req.Skippable,
		Index:         index,
	})
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithBoard(c, b.Key, gin.H{"stage": created}, true)
}

// UpdateStageReq 是修改阶段的请求体，字段为空表示不改。
type UpdateStageReq struct {
	Label         *string `json:"label"          binding:"omitempty,max=20"`
	Kind          *string `json:"kind"           binding:"omitempty,oneof=normal interview terminal_success terminal_fail"`
	Color         *string `json:"color"          binding:"omitempty,max=20"`
	RequiresOwner *bool   `json:"requires_owner"`
	Skippable     *bool   `json:"skippable"`
}

// UpdateStage 改阶段的展示名 / 类型 / 颜色 / 跟进人要求 / 是否可跳过。key 不变，卡片不受影响。
func (h *Handler) UpdateStage(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	var req UpdateStageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	updated, err := h.Stages.Update(c.Request.Context(), id, stage.UpdateInput{
		Label:         req.Label,
		Kind:          req.Kind,
		Color:         req.Color,
		RequiresOwner: req.RequiresOwner,
		Skippable:     req.Skippable,
	})
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithStageBoard(c, updated, gin.H{"stage": updated})
}

// ReorderStageReq 是调整列顺序的请求体。
type ReorderStageReq struct {
	Index int `json:"index"`
}

// ReorderStage 把阶段挪到第 index 列。
func (h *Handler) ReorderStage(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	var req ReorderStageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	moved, err := h.Stages.Reorder(c.Request.Context(), id, req.Index)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithStageBoard(c, moved, gin.H{"stage": moved})
}

// DeleteStage 删除阶段；阶段下还有流程时返回 409。
func (h *Handler) DeleteStage(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	current, err := h.Stages.Get(c.Request.Context(), id)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	if err := h.Stages.Delete(c.Request.Context(), id); err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithStageBoard(c, current, gin.H{"deleted": id})
}

// ---------- 面试流程 ----------

// CreateApplicationReq 是新建面试流程的请求体。
type CreateApplicationReq struct {
	Company  string `json:"company" binding:"required,max=60"`
	Role     string `json:"role"    binding:"max=60"`
	Channel  string `json:"channel" binding:"max=30"`
	Notes    string `json:"notes"   binding:"max=2000"`
	OwnerID  *int64 `json:"owner_id"`
	Intent   string `json:"intent"    binding:"omitempty,oneof=low normal high"`
	StageKey string `json:"stage_key"` // 起始阶段，缺省落在第一列
}

// CreateApplication 在看板下新建一条投递，缺省落在第一个阶段。
func (h *Handler) CreateApplication(c *gin.Context) {
	var req CreateApplicationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	b, err := h.Boards.GetByKey(c.Request.Context(), c.Param("key"))
	if err != nil {
		ginx.Fail(c, err)
		return
	}

	created, err := h.Applications.Create(c.Request.Context(), b.ID, application.CreateInput{
		Company:  req.Company,
		Role:     req.Role,
		Channel:  req.Channel,
		Notes:    req.Notes,
		OwnerID:  req.OwnerID,
		Intent:   req.Intent,
		StageKey: req.StageKey,
	}, ginx.ActorID(c))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithBoard(c, b.Key, gin.H{"application": created}, true)
}

// GetApplication 返回流程详情、面试轮次与操作日志。
func (h *Handler) GetApplication(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	detail, err := h.Applications.GetDetail(c.Request.Context(), id)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	texts := make([]gin.H, 0, len(detail.Events))
	for _, e := range detail.Events {
		texts = append(texts, gin.H{"id": e.ID, "text": e.Text(), "created_at": e.CreatedAt})
	}
	ginx.OK(c, gin.H{"application": detail.Application, "rounds": detail.Rounds, "events": texts})
}

// UpdateApplicationReq 是维护流程信息的请求体，字段为空表示不改。
type UpdateApplicationReq struct {
	Company *string `json:"company" binding:"omitempty,max=60"`
	Role    *string `json:"role"    binding:"omitempty,max=60"`
	Channel *string `json:"channel" binding:"omitempty,max=30"`
	Notes   *string `json:"notes"   binding:"omitempty,max=2000"`
	OwnerID *int64  `json:"owner_id"` // 传 0 表示取消跟进人
	Intent  *string `json:"intent"  binding:"omitempty,oneof=low normal high"`
}

// UpdateApplication 维护流程信息 / 改派跟进人。
func (h *Handler) UpdateApplication(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	var req UpdateApplicationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}

	updated, err := h.Applications.Update(c.Request.Context(), id, application.UpdateInput{
		Company: req.Company,
		Role:    req.Role,
		Channel: req.Channel,
		Notes:   req.Notes,
		OwnerID: req.OwnerID,
		Intent:  req.Intent,
	}, ginx.ActorID(c))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithApplicationBoard(c, updated)
}

// MoveApplicationReq 是阶段流转请求体，拖拽与下拉共用。
type MoveApplicationReq struct {
	To    string `json:"to"    binding:"required"`
	Index *int   `json:"index"` // 目标列内下标，缺省追加到列尾
}

// MoveApplication 执行阶段流转，非法流转由 workflow 拦截并返回 409。
func (h *Handler) MoveApplication(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	var req MoveApplicationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	index := -1
	if req.Index != nil {
		index = *req.Index
	}

	moved, err := h.Applications.Move(c.Request.Context(), id, req.To, index, ginx.ActorID(c))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithApplicationBoard(c, moved)
}

// DeleteApplication 删除一条面试流程，连带它的面试记录与操作日志。
func (h *Handler) DeleteApplication(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	current, err := h.Applications.Get(c.Request.Context(), id)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	if err := h.Applications.Delete(c.Request.Context(), id); err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithBoard(c, current.BoardKey, gin.H{"deleted": id}, false)
}

// ---------- 面试排期 ----------

// RoundReq 是安排 / 修改一场面试的请求体。
// scheduled_at 用 RFC3339；改期时传新时间，传空字符串表示退回「待安排」。
type RoundReq struct {
	ScheduledAt  *string `json:"scheduled_at"`
	DurationMin  *int    `json:"duration_min"  binding:"omitempty,min=5,max=600"`
	Mode         *string `json:"mode"          binding:"omitempty,oneof=online onsite phone"`
	MeetingURL   *string `json:"meeting_url"   binding:"omitempty,max=300"`
	MeetingPlace *string `json:"meeting_place" binding:"omitempty,max=120"`
	Interviewer  *string `json:"interviewer"   binding:"omitempty,max=60"`
	Result       *string `json:"result"        binding:"omitempty,oneof=pending awaiting passed failed cancelled"`
	Notes        *string `json:"notes"         binding:"omitempty,max=2000"`
}

// ScheduleRound 给流程安排一场面试。
func (h *Handler) ScheduleRound(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	var req RoundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	at, _, err := parseSchedule(req.ScheduledAt)
	if err != nil {
		ginx.Fail(c, err)
		return
	}

	created, err := h.Applications.ScheduleRound(c.Request.Context(), id, application.RoundInput{
		ScheduledAt:  at,
		DurationMin:  derefInt(req.DurationMin),
		Mode:         derefString(req.Mode),
		MeetingURL:   derefString(req.MeetingURL),
		MeetingPlace: derefString(req.MeetingPlace),
		Interviewer:  derefString(req.Interviewer),
		Result:       derefString(req.Result),
		Notes:        derefString(req.Notes),
	}, ginx.ActorID(c))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	payload := gin.H{"round": created}
	h.autoSync(c, created.ID, payload)
	h.respondWithRound(c, created.ApplicationID, payload, true)
}

// UpdateRound 改期、补会议信息或回填面试结果。
func (h *Handler) UpdateRound(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	var req RoundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	at, clear, err := parseSchedule(req.ScheduledAt)
	if err != nil {
		ginx.Fail(c, err)
		return
	}

	updated, err := h.Applications.UpdateRound(c.Request.Context(), id, application.RoundUpdateInput{
		ScheduledAt:   at,
		ClearSchedule: clear,
		DurationMin:   req.DurationMin,
		Mode:          req.Mode,
		MeetingURL:    req.MeetingURL,
		MeetingPlace:  req.MeetingPlace,
		Interviewer:   req.Interviewer,
		Result:        req.Result,
		Notes:         req.Notes,
	}, ginx.ActorID(c))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	payload := gin.H{"round": updated}
	h.autoSync(c, updated.ID, payload)
	h.respondWithRound(c, updated.ApplicationID, payload, false)
}

// DeleteRound 删除一条面试记录。
func (h *Handler) DeleteRound(c *gin.Context) {
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.autoUnsync(c, id) // 删库之后就查不到 google_event_id 了，得先撤日程
	applicationID, err := h.Applications.DeleteRound(c.Request.Context(), id, ginx.ActorID(c))
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithRound(c, applicationID, gin.H{"deleted": id}, false)
}

// ---------- 成员 ----------

// ListMembers 返回成员列表与当前用户。
func (h *Handler) ListMembers(c *gin.Context) {
	members, err := h.Members.List(c.Request.Context())
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.OK(c, gin.H{"members": members, "current_member_id": ginx.ActorID(c)})
}

// CreateMemberReq 是新增成员的请求体。
type CreateMemberReq struct {
	Name string `json:"name" binding:"required,max=20"`
	Role string `json:"role" binding:"omitempty,oneof=member lead"`
}

// CreateMember 新增可被指定为跟进人的成员。
func (h *Handler) CreateMember(c *gin.Context) {
	var req CreateMemberReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	created, err := h.Members.Create(c.Request.Context(), req.Name, req.Role)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.Created(c, gin.H{"member": created})
}

// SwitchMemberReq 是切换当前用户的请求体。
type SwitchMemberReq struct {
	MemberID int64 `json:"member_id" binding:"required"`
}

// SwitchMember 切换「我是谁」，写入 cookie，后续操作日志记在该成员名下。
func (h *Handler) SwitchMember(c *gin.Context) {
	var req SwitchMemberReq
	if err := c.ShouldBindJSON(&req); err != nil {
		ginx.Fail(c, err)
		return
	}
	m, err := h.Members.Get(c.Request.Context(), req.MemberID)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.SetCurrentMember(c, m.ID)
	ginx.OK(c, gin.H{"member": m})
}

// ---------- 组装响应 ----------

// respondWithApplicationBoard 在流程变更后，连带返回它所属看板的最新数据。
func (h *Handler) respondWithApplicationBoard(c *gin.Context, a application.Application) {
	h.respondWithBoard(c, a.BoardKey, gin.H{"application": a}, false)
}

// respondWithStageBoard 在阶段配置变更后，连带返回最新看板。
func (h *Handler) respondWithStageBoard(c *gin.Context, st stage.Stage, payload gin.H) {
	b, err := h.Boards.GetByID(c.Request.Context(), st.BoardID)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithBoard(c, b.Key, payload, false)
}

// respondWithRound 在排期变更后，连带返回最新看板与该流程的全部轮次。
func (h *Handler) respondWithRound(c *gin.Context, applicationID int64, payload gin.H, created bool) {
	detail, err := h.Applications.GetDetail(c.Request.Context(), applicationID)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	payload["application"] = detail.Application
	payload["rounds"] = detail.Rounds
	h.respondWithBoard(c, detail.Application.BoardKey, payload, created)
}

// respondWithBoard 把变更结果与最新看板一起返回，前端一次请求即可刷新界面与摘要。
func (h *Handler) respondWithBoard(c *gin.Context, key string, payload gin.H, created bool) {
	f, err := ParseFilter(c)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	view, err := h.Board(c, key, f)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	payload["board"] = view
	if created {
		ginx.Created(c, payload)
		return
	}
	ginx.OK(c, payload)
}

// ---------- 小工具 ----------

// kindOptions 返回阶段类型下拉的选项。
func kindOptions() []gin.H {
	out := make([]gin.H, 0, 4)
	for _, k := range workflow.Kinds() {
		out = append(out, gin.H{"value": string(k), "label": workflow.KindLabel(k)})
	}
	return out
}

// parseSchedule 解析面试时间：nil 表示不改，空字符串表示清空，其余按 RFC3339 解析。
func parseSchedule(raw *string) (at *time.Time, clear bool, err error) {
	if raw == nil {
		return nil, false, nil
	}
	if *raw == "" {
		return nil, true, nil
	}
	parsed, perr := time.Parse(time.RFC3339, *raw)
	if perr != nil {
		return nil, false, apperr.Invalid("scheduled_at", "面试时间格式不合法，需要 RFC3339，如 2026-08-25T14:00:00+08:00")
	}
	return &parsed, false, nil
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
