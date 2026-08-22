// Package ai 封装对 OpenAI Chat Completions 的调用，只做一件事：
// 给一段自由文本 + 一份 JSON Schema，拿回符合 schema 的结构化结果。
//
// 用标准库直接发 HTTP，不引入 SDK：请求体就三个字段，
// 换成任何 OpenAI 兼容网关（Azure、one-api、本地推理）只要改 base URL。
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// 默认配置：不配环境变量也能跑通官方接口。
const (
	DefaultBaseURL = "https://api.openai.com/v1"
	DefaultModel   = "gpt-4o-mini"
	DefaultTimeout = 40 * time.Second
)

// ErrNotConfigured 表示没有配 API key，调用方应把 AI 功能降级掉而不是报 500。
var ErrNotConfigured = errors.New("未配置 OPENAI_API_KEY")

// Config 是客户端配置。
type Config struct {
	APIKey  string
	BaseURL string // 兼容网关改这里，末尾不带斜杠
	Model   string
	Timeout time.Duration
}

// ConfigFromEnv 从环境变量读配置：
// OPENAI_API_KEY / OPENAI_BASE_URL / OPENAI_MODEL。
func ConfigFromEnv() Config {
	return Config{
		APIKey:  strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		BaseURL: strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
		Model:   strings.TrimSpace(os.Getenv("OPENAI_MODEL")),
	}
}

// Request 是一次结构化抽取请求。
type Request struct {
	System     string         // 系统提示词
	User       string         // 待解析的原文
	SchemaName string         // schema 名称，OpenAI 要求 ^[a-zA-Z0-9_-]+$
	Schema     map[string]any // 期望的 JSON Schema
}

// Completer 是业务侧对模型的最小依赖，测试注入假实现即可，不打真网络。
type Completer interface {
	// Complete 返回模型生成的 JSON（已保证是一个对象）。
	Complete(ctx context.Context, req Request) (json.RawMessage, error)
	// Available 表示当前配置是否可用。
	Available() bool
}

// Client 是 Completer 的 OpenAI 实现。
type Client struct {
	cfg  Config
	http *http.Client
}

// New 构造客户端。缺省值在这里补齐，没有 key 时返回的客户端 Available() 为 false。
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

// Available 表示配了 API key。
func (c *Client) Available() bool { return c != nil && c.cfg.APIKey != "" }

// Model 返回实际使用的模型名，用于给前端做提示。
func (c *Client) Model() string { return c.cfg.Model }

// chatRequest 是 /chat/completions 的请求体（只用到需要的字段）。
type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Temperature    float64       `json:"temperature"`
	ResponseFormat any           `json:"response_format"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse 只解析 choices[0].message.content 与 error.message。
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete 调一次模型并返回结构化 JSON。
// 用 structured outputs（response_format=json_schema, strict）约束输出，
// 省掉「模型偶尔多包一层 markdown」这类补丁逻辑。
func (c *Client) Complete(ctx context.Context, req Request) (json.RawMessage, error) {
	if !c.Available() {
		return nil, ErrNotConfigured
	}

	body, err := json.Marshal(chatRequest{
		Model:       c.cfg.Model,
		Temperature: 0,
		Messages: []chatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		ResponseFormat: map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   req.SchemaName,
				"strict": true,
				"schema": req.Schema,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	res, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("调用模型失败: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取模型响应失败: %w", err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("模型响应不是合法 JSON（HTTP %d）: %w", res.StatusCode, err)
	}
	if res.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return nil, fmt.Errorf("模型返回错误（HTTP %d）: %s", res.StatusCode, parsed.Error.Message)
		}
		return nil, fmt.Errorf("模型返回错误（HTTP %d）", res.StatusCode)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("模型没有返回任何结果")
	}
	if refusal := parsed.Choices[0].Message.Refusal; refusal != "" {
		return nil, fmt.Errorf("模型拒绝了这次解析: %s", refusal)
	}

	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if !json.Valid([]byte(content)) {
		return nil, errors.New("模型输出不是合法 JSON")
	}
	return json.RawMessage(content), nil
}
