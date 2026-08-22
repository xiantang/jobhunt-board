// Package stage 是阶段配置模块：每个看板拥有一套可自由增删改序的面试阶段。
// 它是 workflow.Flow 的唯一数据来源——改完配置，下一次操作立刻生效。
package stage

import (
	"strings"

	"interview/internal/workflow"
)

// Stage 是看板上的一列。
type Stage struct {
	ID            int64         `json:"id"`
	BoardID       int64         `json:"board_id"`
	Key           string        `json:"key"`   // 稳定标识，改名不动它
	Label         string        `json:"label"` // 展示名
	Kind          workflow.Kind `json:"kind"`
	KindLabel     string        `json:"kind_label"`
	Color         string        `json:"color"`
	RequiresOwner bool          `json:"requires_owner"`
	Skippable     bool          `json:"skippable"` // 允许被跨过去
	Position      float64       `json:"-"`
}

// CreateInput 是新增阶段的入参。
type CreateInput struct {
	Label         string
	Kind          string
	Color         string
	RequiresOwner bool
	Skippable     bool
	Index         int // 插入到第几列，< 0 表示插到终态阶段之前
}

// UpdateInput 是修改阶段的入参，nil 表示该字段不改。
type UpdateInput struct {
	Label         *string
	Kind          *string
	Color         *string
	RequiresOwner *bool
	Skippable     *bool
}

// Flow 把阶段列表转成流转规则。列表必须已按 position 排好序。
func Flow(stages []Stage) workflow.Flow {
	out := make([]workflow.Stage, 0, len(stages))
	for _, s := range stages {
		out = append(out, workflow.Stage{
			Key:           s.Key,
			Label:         s.Label,
			Kind:          s.Kind,
			RequiresOwner: s.RequiresOwner,
			Skippable:     s.Skippable,
		})
	}
	return workflow.NewFlow(out)
}

// palette 是新增阶段时轮换使用的列头颜色。
var palette = []string{"#2563eb", "#0891b2", "#7c3aed", "#db2777", "#d97706", "#16a34a", "#4b5563"}

// slug 从展示名生成一个稳定的 key。中文标签取不出 ASCII，
// 此时回退到 "stage"，再由 uniqueKey 补上序号。
func slug(label string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteRune('_')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "stage"
	}
	if len(out) > 32 {
		out = strings.Trim(out[:32], "_")
	}
	return out
}
