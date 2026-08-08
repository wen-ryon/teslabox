package teslacam

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestReadMP4Duration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		version   byte
		timescale uint32
		duration  uint64
		want      float64
	}{
		{name: "version zero", version: 0, timescale: 1000, duration: 60500, want: 60.5},
		{name: "version one", version: 1, timescale: 90000, duration: 5400000, want: 60},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "video.mp4")
			if err := os.WriteFile(path, movieHeaderFixture(test.version, test.timescale, test.duration), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readMP4Duration(path)
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(got-test.want) > 0.001 {
				t.Fatalf("duration = %f, want %f", got, test.want)
			}
		})
	}
}

func TestReadMP4DurationPrefersVideoTrackOverContainer(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "video.mp4")
	fixture := makeMP4Box("moov", append(
		movieHeaderBox(1000, 63_000),
		append(
			mediaTrackBox("soun", 48_000, 3_024_000),
			mediaTrackBox("vide", 90_000, 5_400_000)...,
		)...,
	))
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readMP4Duration(path)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-60) > 0.001 {
		t.Fatalf("duration = %f, want video track duration 60", got)
	}
}

func movieHeaderFixture(version byte, timescale uint32, duration uint64) []byte {
	payloadSize := 20
	if version == 1 {
		payloadSize = 32
	}
	payload := make([]byte, payloadSize)
	payload[0] = version
	if version == 0 {
		binary.BigEndian.PutUint32(payload[12:16], timescale)
		binary.BigEndian.PutUint32(payload[16:20], uint32(duration))
	} else {
		binary.BigEndian.PutUint32(payload[20:24], timescale)
		binary.BigEndian.PutUint64(payload[24:32], duration)
	}
	mvhd := makeMP4Box("mvhd", payload)
	return makeMP4Box("moov", mvhd)
}

func movieHeaderBox(timescale uint32, duration uint64) []byte {
	payload := make([]byte, 20)
	binary.BigEndian.PutUint32(payload[12:16], timescale)
	binary.BigEndian.PutUint32(payload[16:20], uint32(duration))
	return makeMP4Box("mvhd", payload)
}

func mediaTrackBox(handler string, timescale uint32, duration uint64) []byte {
	handlerPayload := make([]byte, 12)
	copy(handlerPayload[8:12], handler)
	mediaHeader := make([]byte, 20)
	binary.BigEndian.PutUint32(mediaHeader[12:16], timescale)
	binary.BigEndian.PutUint32(mediaHeader[16:20], uint32(duration))
	media := append(makeMP4Box("hdlr", handlerPayload), makeMP4Box("mdhd", mediaHeader)...)
	return makeMP4Box("trak", makeMP4Box("mdia", media))
}

func makeMP4Box(boxType string, payload []byte) []byte {
	box := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(box[:4], uint32(len(box)))
	copy(box[4:8], boxType)
	copy(box[8:], payload)
	return box
}
