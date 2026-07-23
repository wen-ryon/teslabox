package media

import "math"

// teslaLayoutTiles 让实时播放和导出复用车辆源画面比例，避免固定 16:9 画布给每格补出黑边。
func teslaLayoutTiles(layout string, height int) ([]compositeTile, error) {
	if height <= 0 || height%12 != 0 {
		return nil, &InputError{Message: "合成画布高度无效"}
	}
	switch layout {
	case "composite6":
		rowHeight := height / 2
		columnWidth := teslaAspectWidth(rowHeight)
		return gridTiles(
			[]string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"},
			[]int{columnWidth, columnWidth, columnWidth}, []int{rowHeight, rowHeight},
		), nil
	case "composite4a":
		rowHeight := height / 2
		columnWidth := teslaAspectWidth(rowHeight)
		return gridTiles(
			[]string{"front", "back", "left_repeater", "right_repeater"},
			[]int{columnWidth, columnWidth}, []int{rowHeight, rowHeight},
		), nil
	case "composite4b":
		bottomHeight := height / 4
		topHeight := height - bottomHeight
		columnWidth := teslaAspectWidth(bottomHeight)
		return []compositeTile{
			{camera: "front", width: columnWidth * 3, height: topHeight},
			{camera: "right_repeater", y: topHeight, width: columnWidth, height: bottomHeight},
			{camera: "back", x: columnWidth, y: topHeight, width: columnWidth, height: bottomHeight},
			{camera: "left_repeater", x: columnWidth * 2, y: topHeight, width: columnWidth, height: bottomHeight},
		}, nil
	case "composite3":
		bottomHeight := height / 3
		topHeight := height - bottomHeight
		columnWidth := teslaAspectWidth(bottomHeight)
		return []compositeTile{
			{camera: "front", width: columnWidth * 2, height: topHeight},
			{camera: "right_repeater", y: topHeight, width: columnWidth, height: bottomHeight},
			{camera: "left_repeater", x: columnWidth, y: topHeight, width: columnWidth, height: bottomHeight},
		}, nil
	default:
		return nil, &InputError{Message: "不支持的合成布局"}
	}
}

func teslaAspectWidth(height int) int {
	// 焕新版 Model 3 六路文件均为 724:469；对齐到偶数是 MPP 编码器的硬性要求。
	return int(math.Round(float64(height)*724/469)) &^ 1
}
