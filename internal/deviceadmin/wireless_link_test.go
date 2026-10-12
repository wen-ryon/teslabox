package deviceadmin

import (
	"context"
	"testing"
)

func TestActiveWirelessChannelVendorFallback(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"/usr/sbin/iw dev wlan1 info": "Interface wlan1\n\ttype managed\n",
		"/usr/sbin/iw dev wlan1 link": "Connected to 02:00:00:00:00:01 (on wlan1)\n\tfreq: 5805\n",
	}}
	band, channel := New(runner, nil).activeWirelessChannel(context.Background(), "wlan1")
	if band != "a" || channel != 161 {
		t.Fatalf("厂商关联频率未转换为实际信道: %s/%d", band, channel)
	}
}

func TestIWLinkRequiresActualAssociation(t *testing.T) {
	for _, test := range []struct {
		input   string
		channel int
	}{
		{"Not connected.\n\tfreq: 5805", 0},
		{"Connected to aa (on wlan0)\n\tfreq: 2437", 6},
		{"Connected to aa (on wlan0)\n\tfreq: 5180", 36},
		{"Connected to aa (on wlan0)\n\tfreq: 5955", 0},
		{"Connected to aa (on wlan0)\n\tfreq: 5806", 0},
	} {
		_, got := parseIWLinkChannel(test.input)
		if got != test.channel {
			t.Fatalf("%q: got %d", test.input, got)
		}
	}
}
