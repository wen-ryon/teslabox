package service

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestIndexVolumePartitionIdentity(t *testing.T) {
	for _, tt := range []struct {
		name      string
		offset    uint32
		corrupt   string
		wantError bool
	}{
		{"standard", 2048, "", false}, {"different-offset", 4096, "", false},
		{"invalid-signature", 2048, "signature", true}, {"out-of-bounds", 2048, "bounds", true},
		{"zero-offset", 0, "", true}, {"wrong-filesystem", 2048, "filesystem", true},
		{"gpt-not-supported", 2048, "gpt", true}, {"zero-length", 2048, "empty", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, 3<<20)
			data[510] = 0x55
			data[511] = 0xaa
			entry := data[446:462]
			entry[4] = 7
			binary.LittleEndian.PutUint32(entry[8:12], tt.offset)
			binary.LittleEndian.PutUint32(entry[12:16], 1024)
			if tt.offset > 0 {
				boot := data[int(tt.offset)*512:]
				copy(boot[3:11], "EXFAT   ")
				binary.LittleEndian.PutUint32(boot[100:104], 0x12345678)
				boot[510] = 0x55
				boot[511] = 0xaa
			}
			switch tt.corrupt {
			case "signature":
				data[510] = 0
			case "bounds":
				binary.LittleEndian.PutUint32(entry[12:16], 0xffffffff)
			case "filesystem":
				data[int(tt.offset)*512+3] = 0
			case "gpt":
				entry[4] = 0xee
			case "empty":
				binary.LittleEndian.PutUint32(entry[12:16], 0)
			}
			volume, err := readIndexVolume(bytes.NewReader(data), int64(len(data)))
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v", err)
			}
			if err == nil && (volume.Offset != int64(tt.offset)*512 || volume.Serial != 0x12345678) {
				t.Fatalf("volume=%+v", volume)
			}
		})
	}
}

func TestIndexVolumeRejectsTruncation(t *testing.T) {
	if _, err := readIndexVolume(bytes.NewReader(make([]byte, 100)), 100); err == nil {
		t.Fatal("接受截断镜像")
	}
}
