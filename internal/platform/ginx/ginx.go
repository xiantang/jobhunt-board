// Package ginx 提供 gin 侧的可复用能力：统一响应信封、错误处理中间件、当前用户中间件。
// 业务层（member/board/stage/application/workflow）不 import gin，框架只停留在这一层。
package ginx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"

	"interview/internal/platform/apperr"
)

const (
	// CookieCurrentMember 保存「我是谁」，本轮不做登录，用它表达当前用户。
	CookieCurrentMember = "current_member"
	// ctxCurrentMember 是当前用户在 gin.Context 里的键。
	ctxCurrentMember = "currentMemberID"
)

// errorBody 是统一错误信封的内层结构。
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// OK 输出 200 JSON。
func OK(c *gin.Context, payload any) {
	c.JSON(http.StatusOK, payload)
}

// Created 输出 201 JSON。
func Created(c *gin.Context, payload any) {
	c.JSON(http.StatusCreated, payload)
}

// Fail 把错误挂到 gin.Context，由 ErrorHandler 统一渲染。
// handler 里只需要 `if err != nil { ginx.Fail(c, err); return }`。
func Fail(c *gin.Context, err error) {
	c.Error(err) //nolint:errcheck // gin 的 Error 只会在 err 为 nil 时报错
	c.Abort()
}

// ErrorHandler 在 c.Next() 之后统一把领域错误映射成 HTTP 响应。
func ErrorHandler(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if len(c.Errors) == 0 || c.Writer.Written() {
			return
		}

		err := c.Errors.Last().Err
		appErr := translate(err)
		if appErr.Status >= http.StatusInternalServerError {
			logger.Error("请求处理失败", "path", c.Request.URL.Path, "method", c.Request.Method, "error", err)
		} else {
			logger.Warn("请求被拒绝", "path", c.Request.URL.Path, "code", appErr.Code, "message", appErr.Message)
		}

		c.JSON(appErr.Status, gin.H{"error": errorBody{
			Code:    appErr.Code,
			Message: appErr.Message,
			Field:   appErr.Field,
		}})
	}
}

// translate 把 gin binding 的校验错误翻成中文提示，其余交给 apperr.From。
func translate(err error) *apperr.Error {
	var verrs validator.ValidationErrors
	if errors.As(err, &verrs) && len(verrs) > 0 {
		fe := verrs[0]
		field := jsonName(fe.Field())
		return apperr.Invalid(field, describe(field, fe))
	}
	return apperr.From(err)
}

func describe(field string, fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return fmt.Sprintf("%s 不能为空", field)
	case "max":
		return fmt.Sprintf("%s 长度不能超过 %s", field, fe.Param())
	case "min":
		return fmt.Sprintf("%s 长度不能少于 %s", field, fe.Param())
	case "oneof":
		return fmt.Sprintf("%s 只能是以下之一：%s", field, fe.Param())
	default:
		return fmt.Sprintf("%s 输入不合法", field)
	}
}

// jsonName 把结构体字段名转成前端看得懂的小写下划线形式。
func jsonName(field string) string {
	out := make([]rune, 0, len(field)+4)
	for i, r := range field {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out = append(out, '_')
			}
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return string(out)
}

// ActorResolver 校验 cookie 中的成员 ID 是否可用，不可用时给出兜底成员。
// 由 member.Service 实现，这里只声明所需的最小能力。
type ActorResolver interface {
	ResolveActor(ctx context.Context, id int64) int64
}

// CurrentMember 从 cookie 解析当前用户，并写回 gin.Context。
func CurrentMember(resolver ActorResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		var id int64
		if raw, err := c.Cookie(CookieCurrentMember); err == nil {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				id = parsed
			}
		}
		c.Set(ctxCurrentMember, resolver.ResolveActor(c.Request.Context(), id))
		c.Next()
	}
}

// SetCurrentMember 写入当前用户 cookie（一年有效）。
func SetCurrentMember(c *gin.Context, id int64) {
	c.SetCookie(CookieCurrentMember, strconv.FormatInt(id, 10), 365*24*3600, "/", "", false, true)
	c.Set(ctxCurrentMember, id)
}

// ActorID 返回当前用户 ID，0 表示未识别。
func ActorID(c *gin.Context) int64 {
	if v, ok := c.Get(ctxCurrentMember); ok {
		if id, ok := v.(int64); ok {
			return id
		}
	}
	return 0
}

// PathID 解析路径上的整型参数。
func PathID(c *gin.Context, name string) (int64, error) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.Invalid(name, fmt.Sprintf("%s 必须是正整数", name))
	}
	return id, nil
}
