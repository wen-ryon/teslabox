package teslasei

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadTeslaMetadata(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 0)
	payload = appendVarintField(payload, 1, 2)
	payload = appendVarintField(payload, 2, 1)
	payload = appendVarintField(payload, 3, 42)
	payload = appendFloat32Field(payload, 4, 12.5)
	payload = appendFloat32Field(payload, 6, -4.5)
	payload = appendVarintField(payload, 7, 1)
	payload = appendVarintField(payload, 10, 2)
	payload = appendFloat64Field(payload, 11, 30.12345)
	payload = appendFloat64Field(payload, 12, 120.54321)
	path := filepath.Join(t.TempDir(), "sei.mp4")
	if err := os.WriteFile(path, testMP4(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := Read(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("metadata count = %d", len(items))
	}
	item := items[0]
	if item.Version != 2 || item.Gear != "D" || item.FrameSequence != 42 || item.SpeedMPS != 12.5 || item.SteeringAngle != -4.5 || !item.LeftBlinker || item.Autopilot != "自动辅助转向" {
		t.Fatalf("metadata = %#v", item)
	}
	if math.Abs(item.Latitude-30.12345) > 0.00001 || math.Abs(item.Longitude-120.54321) > 0.00001 {
		t.Fatalf("location = %f, %f", item.Latitude, item.Longitude)
	}
	text := Display(item)
	for _, expected := range []string{"档位 D", "时速 45.0 km/h", "辅助驾驶 自动辅助转向", "GPS 30.12345, 120.54321"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("display missing %q: %s", expected, text)
		}
	}
}

func TestReadReturnsNoMetadataForRegularH264NAL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "without-sei.mp4")
	if err := os.WriteFile(path, testMP4WithoutSEI(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(t.Context(), path); !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("Read() error = %v, want ErrNoMetadata", err)
	}
}

func TestTimelineAndAtUseVideoDuration(t *testing.T) {
	t.Parallel()
	metadata := []Metadata{{FrameSequence: 1}, {FrameSequence: 2}, {FrameSequence: 3}, {FrameSequence: 4}}
	timeline := Timeline(metadata, 4*time.Second, 2)
	if len(timeline) != 2 || timeline[0].OffsetMS != 0 || timeline[1].OffsetMS != 4000 || timeline[1].Metadata.FrameSequence != 4 {
		t.Fatalf("timeline = %#v", timeline)
	}
	item, found := At(metadata, 4*time.Second, 2200*time.Millisecond)
	if !found || item.FrameSequence != 3 {
		t.Fatalf("At() = %#v, %t", item, found)
	}
}

func TestReadHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sei.mp4")
	if err := os.WriteFile(path, testMP4([]byte{0x08, 0x01}), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Read(ctx, path); err != context.Canceled {
		t.Fatalf("Read() error = %v, want context.Canceled", err)
	}
}

func testMP4(payload []byte) []byte {
	nal := append([]byte{0x06, 0x05, 0x20, 'B', 'B', 'B', 'i'}, payload...)
	nal = append(nal, 0x80)
	mdat := make([]byte, 4+len(nal))
	binary.BigEndian.PutUint32(mdat[:4], uint32(len(nal)))
	copy(mdat[4:], nal)
	atom := make([]byte, 8+len(mdat))
	binary.BigEndian.PutUint32(atom[:4], uint32(len(atom)))
	copy(atom[4:8], []byte("mdat"))
	copy(atom[8:], mdat)
	return atom
}

func testMP4WithoutSEI() []byte {
	nal := []byte{0x65, 0x88, 0x99}
	mdat := make([]byte, 4+len(nal))
	binary.BigEndian.PutUint32(mdat[:4], uint32(len(nal)))
	copy(mdat[4:], nal)
	atom := make([]byte, 8+len(mdat))
	binary.BigEndian.PutUint32(atom[:4], uint32(len(atom)))
	copy(atom[4:8], []byte("mdat"))
	copy(atom[8:], mdat)
	return atom
}

func appendVarintField(destination []byte, field int, value uint64) []byte {
	destination = appendVarint(destination, uint64(field<<3))
	return appendVarint(destination, value)
}

func appendFloat32Field(destination []byte, field int, value float32) []byte {
	destination = appendVarint(destination, uint64(field<<3|5))
	buffer := make([]byte, 4)
	binary.LittleEndian.PutUint32(buffer, math.Float32bits(value))
	return append(destination, buffer...)
}

func appendFloat64Field(destination []byte, field int, value float64) []byte {
	destination = appendVarint(destination, uint64(field<<3|1))
	buffer := make([]byte, 8)
	binary.LittleEndian.PutUint64(buffer, math.Float64bits(value))
	return append(destination, buffer...)
}

func appendVarint(destination []byte, value uint64) []byte {
	for value >= 0x80 {
		destination = append(destination, byte(value)|0x80)
		value >>= 7
	}
	return append(destination, byte(value))
}
