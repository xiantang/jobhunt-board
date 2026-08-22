// Package apperr 定义可映射到 HTTP 响应的领域错误。
// service 层只返回 apperr，handler 不需要关心状态码。
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// 错误码：前端可据此做差异化提示。
const (
	CodeValidation        = "VALIDATION_ERROR"
	CodeNotFound          = "NOT_FOUND"
	CodeConflict          = "CONFLICT"
	CodeInvalidTransition = "INVALID_TRANSITION"
	CodeOwnerRequired     = "OWNER_REQUIRED"
	CodeAIUnavailable     = "AI_UNAVAILABLE"
	CodeInternal          = "INTERNAL"
)

// Error 是带 HTTP 语义的领域错误。
type Error struct {
	Code    string // 机器可读错误码
	Status  int    // HTTP 状态码
	Message string // 面向用户的中文提示
	Field   string // 出错字段（校验类错误才有）
	cause   error  // 内部原因，仅记录日志，不返回给前端
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// WithCause 附加内部错误原因。
func (e *Error) WithCause(err error) *Error {
	clone := *e
	clone.cause = err
	return &clone
}

// WithField 标注出错字段。
func (e *Error) WithField(field string) *Error {
	clone := *e
	clone.Field = field
	return &clone
}

// New 构造任意错误。
func New(code string, status int, message string) *Error {
	return &Error{Code: code, Status: status, Message: message}
}

// Invalid 输入校验失败，400。
func Invalid(field, message string) *Error {
	return &Error{Code: CodeValidation, Status: http.StatusBadRequest, Message: message, Field: field}
}

// NotFound 资源不存在，404。
func NotFound(message string) *Error {
	return &Error{Code: CodeNotFound, Status: http.StatusNotFound, Message: message}
}

// Conflict 状态冲突，409。
func Conflict(code, message string) *Error {
	return &Error{Code: code, Status: http.StatusConflict, Message: message}
}

// Unavailable 依赖的外部能力暂时不可用，503。
func Unavailable(code, message string) *Error {
	return &Error{Code: code, Status: http.StatusServiceUnavailable, Message: message}
}

// Internal 内部错误，500；真实原因只写日志。
func Internal(err error) *Error {
	return &Error{Code: CodeInternal, Status: http.StatusInternalServerError, Message: "服务内部错误，请稍后重试", cause: err}
}

// From 把任意 error 归一化成 *Error。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return Internal(err)
}
