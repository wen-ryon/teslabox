package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestCompiledTranslationPreservesControlsAndCode(t *testing.T) {
	input := `<!doctype html><html lang="en"><body><label id="field"> 界面语言 <select name="lang"><option> 默认　中文 </option><option value="zh-CN">中文</option></select></label><input value="中文" name="中文" data-path="中文" placeholder="中文" title="中文" aria-label="中文"><script>const protocol="中文";</script><style>/*中文*/</style><pre>中文</pre><code>中文</code><textarea>中文</textarea></body></html>`
	output, err := localizeCompiledHTML(input, map[string]string{"界面语言": "Interface language", "默认　中文": "Default Chinese", "中文": "Chinese"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{` 界面语言 `} {
		if strings.Contains(output, expected) {
			t.Fatal("混合子节点的文字没有翻译")
		}
	}
	for _, expected := range []string{` Interface language `, `value="默认　中文"> Default Chinese </option>`, `value="zh-CN">Chinese</option>`, `value="中文" name="中文" data-path="中文" placeholder="Chinese" title="Chinese" aria-label="Chinese"`, `const protocol="中文";`, `/*中文*/`, `<pre>中文</pre>`, `<code>中文</code>`, `<textarea>中文</textarea>`} {
		if !strings.Contains(output, expected) {
			t.Errorf("翻译改变了控件或代码边界: %s\n%s", expected, output)
		}
	}
}

func TestCompiledTranslationEscapesUntrustedCharacters(t *testing.T) {
	output, err := localizeCompiledHTML(`<p title="中文">中文</p>`, map[string]string{"中文": `<script>alert("x")</script>`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "<script>") || !strings.Contains(output, "&lt;script&gt;") {
		t.Fatalf("译文被当成 HTML 执行: %s", output)
	}
}

func TestTranslationResourcesShareOneCatalogue(t *testing.T) {
	for _, runtime := range []bool{false, true} {
		w := httptest.NewRecorder()
		serveI18nScript(runtime)(w, httptest.NewRequest(http.MethodGet, "/i18n.js", nil))
		body := w.Body.String()
		if w.Code != 200 || !strings.HasPrefix(body, "globalThis.TeslaBoxEnglish = Object.freeze(") || !strings.Contains(body, "Confirm repair") {
			t.Fatalf("未使用共享词库: %d", w.Code)
		}
		if strings.Contains(body, "globalThis.TeslaBoxI18n =") != runtime {
			t.Fatal("旧资源兼容入口错误")
		}
	}
}

func TestCompiledPagesHaveEnglishForVisibleText(t *testing.T) {
	missing := make(map[string]bool)
	for _, name := range []string{"index.html", "carplay.html"} {
		content, err := fs.ReadFile(assets, "assets/"+name)
		if err != nil {
			t.Fatal(err)
		}
		document, err := html.Parse(strings.NewReader(string(content)))
		if err != nil {
			t.Fatal(err)
		}
		check := func(value string) {
			key := strings.TrimSpace(value)
			if strings.ContainsFunc(key, func(r rune) bool { return r >= 0x3400 && r <= 0x9fff }) {
				if _, ok := englishCatalogue[key]; !ok && key != "简体中文" {
					missing[key] = true
				}
			}
		}
		var walk func(*html.Node)
		walk = func(node *html.Node) {
			if node.Type == html.ElementNode {
				if slices.Contains([]string{"script", "style", "pre", "code", "textarea"}, node.Data) {
					return
				}
				for _, attr := range node.Attr {
					if slices.Contains([]string{"title", "placeholder", "aria-label"}, attr.Key) {
						check(attr.Val)
					}
				}
			}
			if node.Type == html.TextNode {
				check(node.Data)
			}
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				walk(child)
			}
		}
		walk(document)
	}
	keys := make([]string, 0, len(missing))
	for key := range missing {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		t.Errorf("缺少固定文案: %q", key)
	}
}

func TestStaticNASFragmentsHaveEnglishForVisibleText(t *testing.T) {
	content, err := fs.ReadFile(assets, "assets/nas-upload.js")
	if err != nil {
		t.Fatal(err)
	}
	remaining := string(content)
	count := 0
	for {
		start := strings.Index(remaining, "translateUI.html`")
		if start < 0 {
			break
		}
		remaining = remaining[start+len("translateUI.html`"):]
		end := strings.IndexByte(remaining, '`')
		if end < 0 {
			t.Fatal("固定 HTML 片段未闭合")
		}
		fragment := remaining[:end]
		remaining = remaining[end+1:]
		if strings.Contains(fragment, "${") || strings.HasSuffix(fragment, "\\") {
			t.Fatal("固定片段需保持可静态审查，不能混入插值或转义反引号")
		}
		count++
		translated, err := localizeCompiledHTML(fragment, englishCatalogue)
		if err != nil {
			t.Fatal(err)
		}
		document, err := html.Parse(strings.NewReader(translated))
		if err != nil {
			t.Fatal(err)
		}
		check := func(value string) {
			value = strings.TrimSpace(value)
			if strings.ContainsFunc(value, func(r rune) bool { return r >= 0x3400 && r <= 0x9fff }) {
				t.Errorf("固定片段 %d 缺少英文: %q", count, value)
			}
		}
		var walk func(*html.Node)
		walk = func(node *html.Node) {
			if node.Type == html.ElementNode {
				if slices.Contains([]string{"script", "style", "pre", "code", "textarea"}, node.Data) {
					return
				}
				for _, attr := range node.Attr {
					if slices.Contains([]string{"title", "placeholder", "aria-label"}, attr.Key) {
						check(attr.Val)
					}
				}
			}
			if node.Type == html.TextNode {
				check(node.Data)
			}
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				walk(child)
			}
		}
		walk(document)
	}
	if count == 0 {
		t.Fatal("没有找到待验证的 NAS 固定片段")
	}
}
