// Package ordering 提供看板列内 / 列之间通用的浮点排序位置计算。
// 卡片和阶段共用同一套「中点插入」算法：只改动一行，不重排整列。
package ordering

// Gap 是相邻两项之间的默认间距。
const Gap = 1000

// At 根据目标下标算出插入位置；index < 0 或越界表示追加到末尾。
// positions 必须是已排好序、且已排除被移动项自身的现有位置列表。
func At(positions []float64, index int) float64 {
	if len(positions) == 0 {
		return Gap
	}
	if index < 0 || index >= len(positions) {
		return positions[len(positions)-1] + Gap
	}
	if index == 0 {
		return positions[0] - Gap
	}
	return (positions[index-1] + positions[index]) / 2
}
