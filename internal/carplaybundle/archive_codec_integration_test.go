//go:build linux && integration

package carplaybundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCodecDiagnosticPreservesVersionIntegrityAcrossRestart(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("本地集成回归需要 Node.js:", err)
	}
	data := sampleArchive(t, nil, nil)
	sum := sha256.Sum256(data)
	parent := t.TempDir()
	version, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), parent)
	if err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(parent, "session")
	if err := os.Mkdir(session, 0700); err != nil {
		t.Fatal(err)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试源码")
	}
	module := filepath.Join(filepath.Dir(source), "../../experiments/carplay-native/reference-livi/codec-diagnostic.mjs")
	script := `import {pathToFileURL} from 'node:url'; import fs from 'node:fs';
      const [module, directory, runtimeDirectory] = process.argv.slice(1);
      const {writeCodecDiagnostic}=await import(pathToFileURL(module));
      writeCodecDiagnostic(Buffer.from('codec'), {directory:fs.realpathSync(directory), runtimeDirectory:fs.realpathSync(runtimeDirectory), allowInsecureTestPlatform:process.platform!=='linux'});`
	output, err := exec.Command(node, "--input-type=module", "-e", script, module, session, version).CombinedOutput()
	if err != nil {
		t.Fatalf("执行诊断写入失败: %v %s", err, output)
	}
	if _, err := verifyDirectory(version); err != nil {
		t.Fatalf("运行期诊断污染版本目录: %v", err)
	}
	reused, err := stageArchive(bytes.NewReader(data), hex.EncodeToString(sum[:]), parent)
	if err != nil || reused != version {
		t.Fatalf("下一次启动不能复用完整版本: %s %v", reused, err)
	}
	// 完整性门禁继续拒绝旧故障形式，不能靠允许额外诊断成员修复。
	if err := os.WriteFile(filepath.Join(version, "reference-codec-old.bin"), []byte("codec"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyDirectory(version); err == nil {
		t.Fatal("完整性检查错误接受额外成员")
	}
}
