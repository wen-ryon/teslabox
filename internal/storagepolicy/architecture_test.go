package storagepolicy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestConsumersCannotReintroduceLocalThresholdConfiguration 防止业务包重新接收独立阈值并形成第二套公式。
func TestConsumersCannotReintroduceLocalThresholdConfiguration(t *testing.T) {
	t.Parallel()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位架构测试文件")
	}
	internalRoot := filepath.Dir(filepath.Dir(currentFile))
	for _, packageName := range []string{"snapshot", "storageadmin"} {
		packageName := packageName
		t.Run(packageName, func(t *testing.T) {
			set := token.NewFileSet()
			packages, err := parser.ParseDir(set, filepath.Join(internalRoot, packageName), func(info os.FileInfo) bool {
				return filepath.Ext(info.Name()) == ".go" && !strings.HasSuffix(info.Name(), "_test.go")
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, parsedPackage := range packages {
				for _, file := range parsedPackage.Files {
					ast.Inspect(file, func(node ast.Node) bool {
						switch value := node.(type) {
						case *ast.FuncDecl:
							if value.Name.Name == "requiredFreeBytes" {
								t.Errorf("%s 不得重新实现 requiredFreeBytes", set.Position(value.Pos()))
							}
						case *ast.TypeSpec:
							structure, isStruct := value.Type.(*ast.StructType)
							if !isStruct || value.Name.Name != "Config" {
								break
							}
							for _, field := range structure.Fields.List {
								for _, name := range field.Names {
									if name.Name == "MinFreeBytes" || name.Name == "MinFreePercent" || name.Name == "InitializationHeadroomBytes" {
										t.Errorf("%s 的 Config 必须注入 Policy，不能接收独立阈值 %s", packageName, name.Name)
									}
								}
							}
						}
						return true
					})
				}
			}
		})
	}
}
