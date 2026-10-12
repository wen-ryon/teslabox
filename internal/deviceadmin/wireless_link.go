package deviceadmin

import (
	"strconv"
	"strings"
)

func parseIWLinkChannel(output string) (string, int) {
	if !strings.HasPrefix(strings.TrimSpace(output), "Connected to ") {
		return "", 0
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "freq:" {
			continue
		}
		frequency, err := strconv.Atoi(fields[1])
		if err != nil {
			return "", 0
		}
		switch {
		case frequency >= 2412 && frequency <= 2472 && (frequency-2407)%5 == 0:
			return "bg", (frequency - 2407) / 5
		case frequency == 2484:
			return "bg", 14
		case frequency >= 5180 && frequency <= 5885 && (frequency-5000)%5 == 0:
			return "a", (frequency - 5000) / 5
		}
	}
	return "", 0
}
