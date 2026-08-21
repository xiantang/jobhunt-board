// Package workflow 是流程模块：定义面试阶段之间的合法流转规则。
//
// 阶段本身是可配置的（存在 stages 表里，每个看板一套），所以这里不再持有
// 硬编码的状态枚举，而是接收一份阶段快照构造 Flow。规则集中在这里，
// service 与前端都以它为唯一事实来源，包内不 import 数据库。
package workflow

import (
	"fmt"

	"interview/internal/platform/apperr"
)

// Kind 是阶段类型，决定它在流转规则中的特殊待遇。
type Kind string

const (
	KindNormal    Kind = "normal"           // 普通阶段，如 HR screen、在线测评、Offer 审批中
	KindInterview Kind = "interview"        // 面试阶段，移入时自动建一条待安排的面试轮次
	KindOffer     Kind = "terminal_success" // 终态：拿到 Offer
	KindRejected  Kind = "terminal_fail"    // 终态：挂了 / 主动结束
)

// Kinds 按看板顺序返回全部阶段类型，供配置界面渲染下拉。
func Kinds() []Kind { return []Kind{KindNormal, KindInterview, KindOffer, KindRejected} }

var kindLabels = map[Kind]string{
	KindNormal:    "普通阶段",
	KindInterview: "面试阶段",
	KindOffer:     "终态 · Offer",
	KindRejected:  "终态 · 结束",
}

// KindLabel 返回阶段类型的中文名。
func KindLabel(k Kind) string {
	if l, ok := kindLabels[k]; ok {
		return l
	}
	return string(k)
}

// ParseKind 校验并转换外部传入的阶段类型。
func ParseKind(raw string) (Kind, error) {
	k := Kind(raw)
	if _, ok := kindLabels[k]; !ok {
		return "", apperr.Invalid("kind", fmt.Sprintf("未知的阶段类型：%s", raw))
	}
	return k, nil
}

// Terminal 判断是否为终态阶段（Offer 或结束）。
func (k Kind) Terminal() bool { return k == KindOffer || k == KindRejected }

// Stage 是流转规则需要知道的阶段信息，由 stage 包从数据库加载后传进来。
type Stage struct {
	Key           string `json:"key"`
	Label         string `json:"label"`
	Kind          Kind   `json:"kind"`
	Position      int    `json:"position"` // 在看板中的序号，0 起
	RequiresOwner bool   `json:"requires_owner"`
}

// Flow 是某个看板的一套阶段流转规则。零值不可用，必须经 NewFlow 构造。
type Flow struct {
	stages []Stage
	index  map[string]int // stage key -> stages 下标
}

// NewFlow 用一份按展示顺序排好的阶段列表构造流转规则。
// Position 以列表下标为准，调用方不需要自己填。
func NewFlow(stages []Stage) Flow {
	out := make([]Stage, len(stages))
	index := make(map[string]int, len(stages))
	for i, s := range stages {
		s.Position = i
		out[i] = s
		index[s.Key] = i
	}
	return Flow{stages: out, index: index}
}

// Stages 返回全部阶段（按看板列顺序）。
func (f Flow) Stages() []Stage {
	out := make([]Stage, len(f.stages))
	copy(out, f.stages)
	return out
}

// Empty 判断这个看板还没有配置任何阶段。
func (f Flow) Empty() bool { return len(f.stages) == 0 }

// Entry 返回第一个阶段，新投递默认落在这里。
func (f Flow) Entry() (Stage, error) {
	if len(f.stages) == 0 {
		return Stage{}, apperr.Conflict(apperr.CodeConflict, "该看板还没有配置任何阶段，请先添加阶段")
	}
	return f.stages[0], nil
}

// Parse 校验并转换外部传入的阶段 key。
func (f Flow) Parse(raw string) (Stage, error) {
	i, ok := f.index[raw]
	if !ok {
		return Stage{}, apperr.Invalid("stage", fmt.Sprintf("未知的面试阶段：%s", raw))
	}
	return f.stages[i], nil
}

// Can 判断 from → to 是否为合法流转：
//
//  1. 同阶段视为合法（只是调整卡片排序）
//  2. 任何阶段都可以直达终态——随时可能拿到 Offer 或被挂
//  3. 终态可以退回任意非终态——用来撤销误标
//  4. 其余只允许相邻阶段进/退一格，禁止跨阶段跳跃
func (f Flow) Can(from, to Stage) bool {
	switch {
	case from.Key == to.Key:
		return true
	case to.Kind.Terminal():
		return true
	case from.Kind.Terminal():
		return true
	default:
		return abs(to.Position-from.Position) == 1
	}
}

// Next 返回某阶段可流转到的阶段列表，供前端渲染按钮。
func (f Flow) Next(from Stage) []Stage {
	out := make([]Stage, 0, 4)
	for _, s := range f.stages {
		if s.Key == from.Key || !f.Can(from, s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Validate 校验一次阶段流转，返回可直接响应给前端的领域错误。
// hasOwner 用于 RequiresOwner 这条可配置规则：某些阶段必须先指定跟进人。
func (f Flow) Validate(from, to Stage, hasOwner bool) error {
	if !f.Can(from, to) {
		return apperr.Conflict(apperr.CodeInvalidTransition,
			fmt.Sprintf("不允许从「%s」直接流转到「%s」", from.Label, to.Label))
	}
	if to.RequiresOwner && from.Key != to.Key && !hasOwner {
		return apperr.Conflict(apperr.CodeOwnerRequired,
			fmt.Sprintf("「%s」要求先指定跟进人", to.Label))
	}
	return nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
