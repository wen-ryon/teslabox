package updater

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func capabilityELF(t *testing.T, rodata string, writable bool, trailer string) []byte {
	t.Helper()
	names := []byte("\x00.shstrtab\x00.rodata\x00")
	header := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(elf.EM_AARCH64), Version: 1, Ehsize: 64, Shoff: 64, Shentsize: 64, Shnum: 3, Shstrndx: 1}
	copy(header.Ident[:], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	flags := uint64(elf.SHF_ALLOC)
	if writable {
		flags |= uint64(elf.SHF_WRITE)
	}
	sections := []elf.Section64{{}, {Name: 1, Type: uint32(elf.SHT_STRTAB), Off: 256, Size: uint64(len(names)), Addralign: 1},
		{Name: uint32(bytes.Index(names, []byte(".rodata"))), Type: uint32(elf.SHT_PROGBITS), Flags: flags, Off: 256 + uint64(len(names)), Size: uint64(len(rodata)), Addralign: 1}}
	var data bytes.Buffer
	if err := binary.Write(&data, binary.LittleEndian, header); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&data, binary.LittleEndian, sections); err != nil {
		t.Fatal(err)
	}
	data.Write(names)
	data.WriteString(rodata)
	data.WriteString(trailer)
	return data.Bytes()
}

func TestPackageCapabilityIsAuthenticatedReadonlyELFData(t *testing.T) {
	for _, name := range []string{"protected", "legacy", "writable", "trailer only", "duplicate", "bad digest"} {
		t.Run(name, func(t *testing.T) {
			declaration := ProgramTransactionCapability
			trailer := ""
			if name == "legacy" {
				declaration = "old program"
			}
			if name == "trailer only" {
				declaration = "old program"
				trailer = ProgramTransactionCapability
			}
			if name == "duplicate" {
				declaration += declaration
			}
			data := capabilityELF(t, declaration, name == "writable", trailer)
			root := t.TempDir()
			path := filepath.Join(root, "candidate")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			request := InstallRequest{Version: "0.2.0-rc.1", PackageName: "candidate", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
			if name == "bad digest" {
				request.SHA256 = strings.Repeat("a", 64)
			}
			err := verifyLocalPackage(path, request)
			if (err == nil) != (name == "protected") {
				t.Fatalf("verify = %v", err)
			}
			if name == "legacy" {
				body, _ := json.Marshal(request)
				req := filepath.Join(root, "request.json")
				if err = os.WriteFile(req, body, 0o600); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(root, "installed")
				if err = os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg := HelperConfig{RequestPath: req, ResultPath: filepath.Join(root, "result.json"), DownloadDir: root, BackupDir: filepath.Join(root, "backups"), TargetPath: target}
				if err = RunHelper(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "状态回滚保护") {
					t.Fatalf("应在停止前拒绝旧程序: %v", err)
				}
				if _, err = os.Stat(cfg.BackupDir); !os.IsNotExist(err) {
					t.Fatal("能力拒绝不应进入备份/安装阶段")
				}
				got, _ := os.ReadFile(target)
				if string(got) != "preserve" {
					t.Fatal("旧程序被覆盖")
				}
			}
		})
	}
}
