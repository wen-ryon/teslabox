// CarPlay 协议栈验证原型。
//
// 这是一个独立的 Go module，不参与主项目构建：
// 主项目 `go build ./...` 与 golangci-lint 都不会碰到它，
// 成熟之后再决定要不要并入 internal/。
module github.com/wen-ryon/teslabox/experiment/carplay-demo

go 1.26.0
