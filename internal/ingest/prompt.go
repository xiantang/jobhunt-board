package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// systemPrompt 组装系统提示词。
// 阶段清单是动态的——用户随时能增删列，提示词必须跟着当前配置走，
// 否则模型会返回一个这个看板上根本不存在的 stage_key。
func systemPrompt(stages []stageBrief, now time.Time) string {
	list, _ := json.Marshal(stages)

	var b strings.Builder
	b.WriteString("你是一个求职进度助手。用户会粘贴一段与求职有关的文本（多半是 HR 邮件、面试邀约、测评通知或笔试链接），")
	b.WriteString("你要把它抽取成结构化字段，用来在面试看板上录入一条记录。\n\n")

	b.WriteString("当前看板的阶段列表（按流程先后排序，stage_key 只能从中选）：\n")
	b.Write(list)
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "当前时间是 %s（%s）。文本里的相对时间（「下周三」「明天下午三点」）都按这个基准换算。\n\n",
		now.Format("2006-01-02 15:04"), now.Format("Z07:00"))

	b.WriteString(`抽取规则：
- company：公司名。用文本里出现的正式写法，去掉「有限公司」之类的后缀也可以，但不要自己翻译或改写大小写。抽不到就留空字符串。
- role：应聘岗位。文本里没提就留空，不要猜。
- channel：投递渠道，如「官网」「内推」「猎头」「BOSS 直聘」。看不出来就留空。
- stage_key：这封文本对应看板上的哪一列。在线测评 / 笔试 / OA 选测评类的列；一面 / 二面 / 技术面选对应轮次的面试列；发 Offer 选 terminal_success 类的列；拒信选 terminal_fail 类的列。判断不了就留空字符串。
- round.scheduled_at：确定的面试开始时间，RFC3339 带时区。只有文本里给了具体时刻才填，「本周内完成」这种没有时刻的一律填 null。
- round.deadline：截止日期，YYYY-MM-DD。测评链接的过期时间、要求几号前完成，填这里。它不是面试时间，不要塞进 scheduled_at。
- round.duration_min：时长（分钟），没提就填 0。
- round.mode：online（线上会议 / 在线测评 / 电话之外的远程）、onsite（到公司现场）、phone（电话）。判断不了就填 online。
- round.meeting_url：会议链接或测评链接的完整 URL。文本里只有「点击这里」这种没有实际地址的锚文本时，留空字符串。
- round.meeting_place：线下地址或会议室。
- round.interviewer：面试官姓名。
- round.notes：这一轮的备注，一到两句话，写清楚要做什么、有什么要求。
- notes：给卡片本身的备注，一句话概括这次沟通。
- summary：一句话告诉用户「这是一封什么邮件」，中文。
- confidence：high 表示公司和阶段都很明确；medium 表示有一项靠推断；low 表示文本信息太少。

只输出抽到的内容，不要编造文本里没有的信息。所有字符串字段抽不到时都填空字符串，不要填「未知」「N/A」。`)

	return b.String()
}

// extractionSchema 是 structured outputs 的 JSON Schema。
// strict 模式要求每个属性都出现在 required 里，可空字段用 ["string", "null"] 表达。
func extractionSchema() map[string]any {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	nullableStr := func(desc string) map[string]any {
		return map[string]any{"type": []string{"string", "null"}, "description": desc}
	}

	round := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scheduled_at":  nullableStr("确定的面试开始时间，RFC3339；没有具体时刻则为 null"),
			"deadline":      nullableStr("截止日期 YYYY-MM-DD；没有则为 null"),
			"duration_min":  map[string]any{"type": "integer", "description": "时长（分钟），未提及填 0"},
			"mode":          map[string]any{"type": "string", "enum": []string{"online", "onsite", "phone"}},
			"meeting_url":   str("会议 / 测评链接，没有实际 URL 时为空字符串"),
			"meeting_place": str("线下地点或会议室"),
			"interviewer":   str("面试官姓名"),
			"notes":         str("这一轮的备注"),
		},
		"required": []string{
			"scheduled_at", "deadline", "duration_min", "mode",
			"meeting_url", "meeting_place", "interviewer", "notes",
		},
		"additionalProperties": false,
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"company":    str("公司名"),
			"role":       str("应聘岗位"),
			"channel":    str("投递渠道"),
			"stage_key":  str("看板阶段 key，必须来自给定列表；判断不了填空字符串"),
			"notes":      str("给卡片的备注"),
			"summary":    str("一句话说明这是什么文本"),
			"confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
			"round":      round,
		},
		"required": []string{
			"company", "role", "channel", "stage_key",
			"notes", "summary", "confidence", "round",
		},
		"additionalProperties": false,
	}
}

// extraction 对应上面的 schema。
type extraction struct {
	Company    string `json:"company"`
	Role       string `json:"role"`
	Channel    string `json:"channel"`
	StageKey   string `json:"stage_key"`
	Notes      string `json:"notes"`
	Summary    string `json:"summary"`
	Confidence string `json:"confidence"`
	Round      struct {
		ScheduledAt  *string `json:"scheduled_at"`
		Deadline     *string `json:"deadline"`
		DurationMin  int     `json:"duration_min"`
		Mode         string  `json:"mode"`
		MeetingURL   string  `json:"meeting_url"`
		MeetingPlace string  `json:"meeting_place"`
		Interviewer  string  `json:"interviewer"`
		Notes        string  `json:"notes"`
	} `json:"round"`
}
