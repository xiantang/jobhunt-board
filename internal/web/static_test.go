package web_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 页面脚本跑出运行时错误时，整个 IIFE 会静默中断——页面一片空白，
// 而 Go 侧的接口测试全绿（数据确实产出来了，只是没人渲染）。
// 这里用一个最小 DOM 桩把脚本实跑一遍，专门堵这个洞。
//
// 需要本机有 node；没有就跳过，不给 CI 添硬依赖。
func TestCalendarScriptRenders(t *testing.T) {
	node := requireNode(t)

	agenda := map[string]any{
		"from": "2026-08-22T00:00:00+08:00",
		"to":   "2026-08-29T00:00:00+08:00",
		"days": []any{
			map[string]any{
				"date": "2026-08-24", "weekday": "周一", "today": true,
				"entries": []any{
					map[string]any{
						"source": "interview", "title": "Bitdeer · 一面",
						"start": "2026-08-24T09:00:00+08:00", "end": "2026-08-24T10:00:00+08:00",
						"round_id": 22, "application_id": 15, "application_key": "JOBHUNT-11",
						"stage_color": "#2563eb", "mode": "online", "result": "pending",
						"synced": false, "conflict": true,
					},
					map[string]any{
						"source": "google", "title": "组内周会",
						"start": "2026-08-24T09:30:00+08:00", "end": "2026-08-24T10:30:00+08:00",
						"conflict": true,
					},
				},
			},
		},
		"unscheduled": []any{},
		"status":      map[string]any{"enabled": true, "connected": true},
	}

	html := runScript(t, node, "static/calendar.js", agenda, "agenda")

	for _, want := range []string{"Bitdeer · 一面", "组内周会", "ev--google", "week__now"} {
		if !strings.Contains(html, want) {
			t.Fatalf("渲染结果里缺少 %q：\n%s", want, html)
		}
	}

	// 09:00 那场从第 9 小时起画：HOUR=44px，top 该是 396px。
	// 这个数错了，整页事件就会和左边的小时刻度错位——网格视图里这是致命的。
	if !strings.Contains(html, "top:396.0px") {
		t.Fatalf("09:00 的面试该定位在 top:396.0px（9 × 44）：\n%s", html)
	}
	// 两场重叠 → 各占一半宽度，并排放。这是网格视图取代「撞期」角标的方式。
	if got := strings.Count(html, "width:50.000%"); got != 2 {
		t.Fatalf("重叠的两场该各占一半宽度，实际 %d 块：\n%s", got, html)
	}
	if !strings.Contains(html, "left:50.000%") {
		t.Fatalf("重叠的第二场该挪到右半边：\n%s", html)
	}
}

// 注：看板页的 board.js 没在这里跑。它依赖真实的 <form> 具名控件
// （confirmForm.create_round 之类），最小 DOM 桩伪造不出来，硬伪造只会
// 测出一个和浏览器不一样的东西。要覆盖它得上 jsdom——为一个页面加一条
// Node 依赖链不划算，先用日程页这条守住「一进页面就抛异常」这类问题。

func requireNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("本机没有 node，跳过页面脚本冒烟测试")
	}
	return node
}

func runScript(t *testing.T, node, script string, data any, rootID string) string {
	t.Helper()
	out, err := execScript(t, node, script, data, rootID)
	if err != nil {
		t.Fatalf("%s 跑挂了: %v\n%s", script, err, out)
	}
	return out
}

func execScript(t *testing.T, node, script string, data any, rootID string) (string, error) {
	t.Helper()

	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("序列化测试数据失败: %v", err)
	}
	dataPath := filepath.Join(t.TempDir(), "data.json")
	if err := writeFile(dataPath, raw); err != nil {
		t.Fatalf("写入测试数据失败: %v", err)
	}

	cmd := exec.Command(node, "testdata/dom_shim.js", script, dataPath, rootID)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
