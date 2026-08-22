package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"interview/internal/board"
	"interview/internal/calendar"
	"interview/internal/platform/apperr"
	"interview/internal/platform/ginx"
)

// cookieOAuthState 保存 OAuth 的 state，回调时比对，防 CSRF。
const cookieOAuthState = "google_oauth_state"

// GoogleStatus 返回 Google 日历的连接状态。
func (h *Handler) GoogleStatus(c *gin.Context) {
	if h.Calendar == nil {
		ginx.OK(c, gin.H{"status": calendar.Status{}})
		return
	}
	status, err := h.Calendar.Status(c.Request.Context())
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.OK(c, gin.H{"status": status})
}

// GoogleConnect 把用户送去 Google 授权页。
func (h *Handler) GoogleConnect(c *gin.Context) {
	if h.Calendar == nil {
		ginx.Fail(c, apperr.Unavailable(apperr.CodeGoogleNotConnected, "没有配置 Google OAuth 客户端"))
		return
	}
	state, err := randomState()
	if err != nil {
		ginx.Fail(c, apperr.Internal(err))
		return
	}
	url, err := h.Calendar.AuthURL(state)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	// state 只在这一次授权往返里有效，10 分钟足够。
	c.SetCookie(cookieOAuthState, state, 600, "/", "", false, true)
	c.Redirect(http.StatusFound, url)
}

// GoogleCallback 处理 Google 的回调：校验 state、换 token、回日程页。
func (h *Handler) GoogleCallback(c *gin.Context) {
	if h.Calendar == nil {
		ginx.Fail(c, apperr.Unavailable(apperr.CodeGoogleNotConnected, "没有配置 Google OAuth 客户端"))
		return
	}
	back := "/boards/" + defaultBoardKey(c) + "/calendar"

	if reason := c.Query("error"); reason != "" {
		c.Redirect(http.StatusFound, back+"?google=denied")
		return
	}
	want, err := c.Cookie(cookieOAuthState)
	if err != nil || want == "" || want != c.Query("state") {
		c.Redirect(http.StatusFound, back+"?google=state_mismatch")
		return
	}
	c.SetCookie(cookieOAuthState, "", -1, "/", "", false, true)

	if _, err := h.Calendar.Connect(c.Request.Context(), c.Query("code")); err != nil {
		c.Redirect(http.StatusFound, back+"?google=failed")
		return
	}
	c.Redirect(http.StatusFound, back+"?google=connected")
}

// GoogleDisconnect 断开连接。
func (h *Handler) GoogleDisconnect(c *gin.Context) {
	if h.Calendar == nil {
		ginx.Fail(c, apperr.Unavailable(apperr.CodeGoogleNotConnected, "没有配置 Google OAuth 客户端"))
		return
	}
	if err := h.Calendar.Disconnect(c.Request.Context()); err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.OK(c, gin.H{"disconnected": true})
}

// GetAgenda 返回日程数据：看板的面试 + Google 日历的会议，按天铺开。
func (h *Handler) GetAgenda(c *gin.Context) {
	agenda, err := h.Agenda(c)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	ginx.OK(c, agenda)
}

// Agenda 组装日程页数据，页面与接口共用。
// from 缺省是今天，days 缺省一周。
func (h *Handler) Agenda(c *gin.Context) (calendar.Agenda, error) {
	if h.Calendar == nil {
		return calendar.Agenda{}, apperr.Unavailable(apperr.CodeGoogleNotConnected, "日程功能未启用")
	}

	from := time.Now()
	if raw := c.Query("from"); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, time.Local)
		if err != nil {
			return calendar.Agenda{}, apperr.Invalid("from", "起始日期格式应为 2006-01-02")
		}
		from = parsed
	}

	days := calendar.DefaultWindowDays
	if raw := c.Query("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 60 {
			return calendar.Agenda{}, apperr.Invalid("days", "天数需要在 1~60 之间")
		}
		days = parsed
	}
	return h.Calendar.Load(c.Request.Context(), from, days)
}

// SyncRound 手动把一轮面试推到 Google 日历。
func (h *Handler) SyncRound(c *gin.Context) {
	if h.Calendar == nil {
		ginx.Fail(c, apperr.Unavailable(apperr.CodeGoogleNotConnected, "日程功能未启用"))
		return
	}
	id, err := ginx.PathID(c, "id")
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	if err := h.Calendar.SyncRound(c.Request.Context(), id); err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithRoundAfterSync(c, id)
}

// respondWithRoundAfterSync 同步后把最新的轮次数据带回去，前端好更新「已同步」标记。
func (h *Handler) respondWithRoundAfterSync(c *gin.Context, roundID int64) {
	round, err := h.Applications.RoundForSync(c.Request.Context(), roundID)
	if err != nil {
		ginx.Fail(c, err)
		return
	}
	h.respondWithRound(c, round.ApplicationID, gin.H{"synced": round.GoogleEventID != ""}, false)
}

// autoSync 排期变动后尽力把这一轮推到 Google 日历。
//
// 失败不让看板操作跟着失败：日历是外部系统，它挂了不该导致排期存不下来。
// 出错只把原因挂进响应，前端弹一句提示，用户可以在日程页手动重试。
func (h *Handler) autoSync(c *gin.Context, roundID int64, payload gin.H) {
	if h.Calendar == nil || !h.Calendar.Connected(c.Request.Context()) {
		return
	}
	if err := h.Calendar.SyncRound(c.Request.Context(), roundID); err != nil {
		payload["calendar_warning"] = apperr.From(err).Message
	}
}

// autoUnsync 在轮次被删掉之前撤掉它的日程。删库之后就查不到 event id 了。
func (h *Handler) autoUnsync(c *gin.Context, roundID int64) {
	if h.Calendar == nil || !h.Calendar.Connected(c.Request.Context()) {
		return
	}
	_ = h.Calendar.RemoveRound(c.Request.Context(), roundID)
}

// defaultBoardKey 取回调要跳回哪个看板的日程页。
// OAuth 回调地址是固定的，带不上看板 key，所以从 query 里捞，捞不到用默认看板。
func defaultBoardKey(c *gin.Context) string {
	if key := c.Query("board"); key != "" {
		return key
	}
	return board.DefaultKey
}

func randomState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
