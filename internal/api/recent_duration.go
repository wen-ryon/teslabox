package api

// recentDurationDisplayKnown 将时长展示与其他视角的读取失败分开，且不能把不同视角拼凑为一路完整录像。
func recentDurationDisplayKnown(session videoSession) bool {
	if session.ClipType != "recent" || len(session.Moments) == 0 {
		return false
	}
	complete := make(map[string]bool)
	for index, moment := range session.Moments {
		var longest float64
		for _, video := range moment.Videos {
			if video.DurationState != "unavailable" && video.Duration > longest {
				longest = video.Duration
			}
		}
		current := make(map[string]bool)
		for _, video := range moment.Videos {
			// 同分钟各视角的时间戳已有分组容差；秒级展示也不能因不足一秒的尾帧差异隐藏完整时长。
			if video.DurationState == "verified" && video.Duration > 0 && video.Duration+1 >= longest {
				current[video.Camera] = true
			}
		}
		if index == 0 {
			complete = current
		} else {
			for camera := range complete {
				if !current[camera] {
					delete(complete, camera)
				}
			}
		}
		if len(complete) == 0 {
			return false
		}
	}
	return len(complete) > 0
}
