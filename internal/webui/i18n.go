package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/html"
)

var englishCatalogue, englishCatalogueScript = loadEnglishCatalogue()

func loadEnglishCatalogue() (map[string]string, []byte) {
	content, err := assets.ReadFile("assets/i18n-en.json")
	if err != nil {
		panic("编译期英文词库缺失: " + err.Error())
	}
	var catalogue map[string]string
	if err := json.Unmarshal(content, &catalogue); err != nil || len(catalogue) == 0 {
		panic("编译期英文词库无效")
	}
	for key, value := range catalogue {
		if key == "" || value == "" {
			panic("编译期英文词库包含空文案")
		}
	}
	// 保留 JSON 的 HTML 转义，不能让翻译文本成为可执行脚本。
	encoded, err := json.Marshal(catalogue)
	if err != nil {
		panic("编译期英文词库无法编码: " + err.Error())
	}
	script := append([]byte("globalThis.TeslaBoxEnglish = Object.freeze("), encoded...)
	script = append(script, []byte(");\n")...)
	return catalogue, script
}

func serveI18nScript(includeRuntime bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(englishCatalogueScript)
		if includeRuntime {
			runtime, err := assets.ReadFile("assets/i18n.js")
			if err != nil {
				return
			}
			_, _ = w.Write(runtime)
		}
	}
}

func translateCompiledText(value string, catalogue map[string]string) string {
	key := strings.TrimSpace(value)
	translated, ok := catalogue[key]
	if !ok || key == "" {
		return value
	}
	start := strings.Index(value, key)
	return value[:start] + translated + value[start+len(key):]
}

// 此入口只处理二进制内的固定模板；不能传入已经混入 SSID、文件名或日志的页面。
func localizeCompiledHTML(page string, catalogue map[string]string) (string, error) {
	document, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return "", err
	}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "script", "style", "pre", "code", "textarea":
				return
			case "option":
				hasValue := false
				for _, attr := range node.Attr {
					hasValue = hasValue || attr.Key == "value"
				}
				if !hasValue {
					var text strings.Builder
					var collect func(*html.Node)
					collect = func(child *html.Node) {
						if child.Type == html.TextNode {
							text.WriteString(child.Data)
						}
						for next := child.FirstChild; next != nil; next = next.NextSibling {
							collect(next)
						}
					}
					collect(node)
					// HTML 的隐式 option 值只折叠 ASCII 空白，不使用 Unicode Fields 改变协议值。
					value := strings.Join(strings.FieldsFunc(text.String(), func(r rune) bool {
						return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f'
					}), " ")
					node.Attr = append(node.Attr, html.Attribute{Key: "value", Val: value})
				}
			}
			for i := range node.Attr {
				switch node.Attr[i].Key {
				case "title", "placeholder", "aria-label":
					node.Attr[i].Val = translateCompiledText(node.Attr[i].Val, catalogue)
				}
			}
		}
		if node.Type == html.TextNode {
			node.Data = translateCompiledText(node.Data, catalogue)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	var output bytes.Buffer
	if err := html.Render(&output, document); err != nil {
		return "", err
	}
	return output.String(), nil
}

// 固定模板翻译失败时不返回部分 HTML，避免浏览器执行不完整的控件标记。
func writeLocalizedPage(w http.ResponseWriter, page, language string) {
	if language == "en" {
		translated, err := localizeCompiledHTML(page, englishCatalogue)
		if err != nil {
			http.Error(w, "Page resource unavailable", http.StatusInternalServerError)
			return
		}
		page = translated
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, page)
}
