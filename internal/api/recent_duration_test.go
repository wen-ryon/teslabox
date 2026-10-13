package api

import (
	"math"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/teslacam"
)

func TestRecentDurationDisplayWithMissingViews(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	video := func(camera, state string, at time.Time, seconds float64) teslacam.Video {
		return teslacam.Video{Camera: camera, DurationState: state, RecordedAt: at, Duration: seconds}
	}
	for _, test := range []struct {
		name          string
		clipType      string
		first, second []teslacam.Video
		want          bool
		wantDuration  float64
	}{
		{name: "同一视角完整且其他视角损坏", clipType: "recent", want: true,
			first:  []teslacam.Video{video("front", "verified", start, 60), video("back", "unavailable", start, 60)},
			second: []teslacam.Video{video("front", "verified", start.Add(time.Minute), 60)}},
		{name: "不同分钟可读视角不同", clipType: "recent",
			first:  []teslacam.Video{video("front", "verified", start, 60), video("back", "unavailable", start, 60)},
			second: []teslacam.Video{video("back", "verified", start.Add(time.Minute), 60)}},
		{name: "同分钟各视角长度相差一秒", clipType: "recent", want: true, wantDuration: 121,
			first:  []teslacam.Video{video("front", "verified", start, 60), video("back", "verified", start, 61), video("left_pillar", "unavailable", start, 60)},
			second: []teslacam.Video{video("front", "verified", start.Add(time.Minute), 60)}},
		{name: "同分钟各视角尾帧和起点存在差异", clipType: "recent", want: true, wantDuration: 120.8,
			first:  []teslacam.Video{video("front", "verified", start.Add(time.Second), 59.8), video("back", "verified", start, 60), video("left_pillar", "unavailable", start, 60)},
			second: []teslacam.Video{video("front", "verified", start.Add(time.Minute), 60)}},
		{name: "视角存在但长度不足", clipType: "recent",
			first:  []teslacam.Video{video("front", "verified", start, 30), video("back", "verified", start, 60), video("left_pillar", "unavailable", start, 60)},
			second: []teslacam.Video{video("front", "verified", start.Add(time.Minute), 60)}},
		{name: "视角时长尚未核对", clipType: "recent",
			first:  []teslacam.Video{video("front", "estimated", start, 60), video("back", "unavailable", start, 60)},
			second: []teslacam.Video{video("front", "verified", start.Add(time.Minute), 60)}},
		{name: "全部文件不可读", clipType: "recent",
			first:  []teslacam.Video{video("front", "unavailable", start, 60)},
			second: []teslacam.Video{video("front", "unavailable", start.Add(time.Minute), 60)}},
		{name: "已保存录像沿用既有规则", clipType: "saved",
			first:  []teslacam.Video{video("front", "verified", start, 60), video("back", "unavailable", start, 60)},
			second: []teslacam.Video{video("front", "verified", start.Add(time.Minute), 60)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := videoSession{ClipType: test.clipType, StartAt: start, Moments: []videoGroup{
				{RecordedAt: start, Videos: test.first},
				{RecordedAt: start.Add(time.Minute), Videos: test.second},
			}}
			refineVideoSessionDurations("", &session)
			if session.DurationDisplayKnown != test.want {
				t.Fatalf("可显示时长 = %v，期望 %v", session.DurationDisplayKnown, test.want)
			}
			if session.DurationState != "unavailable" {
				t.Fatalf("文件缺失状态被丢弃：%s", session.DurationState)
			}
			wantDuration := 120.0
			if test.wantDuration > 0 {
				wantDuration = test.wantDuration
			}
			if math.Abs(session.Duration-wantDuration) > 0.000001 {
				t.Fatalf("录像时间轴被改变：%v", session.Duration)
			}
		})
	}
}
