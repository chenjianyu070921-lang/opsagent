package report

import (
	"bytes"
	_ "embed"
	"html"
	"html/template"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/yuin/goldmark"
)

//go:embed report.html
var reportHTMLTemplate string

// evMarkerPattern 匹配行内证据标记 [ev-数字]。
var evMarkerPattern = regexp.MustCompile(`\[(ev-\d+)\]`)

// HTMLRenderer 实现 HTML 渲染。
type HTMLRenderer struct{}

// Render 实现 Renderer。
func (HTMLRenderer) Render(w io.Writer, doc *Document) error {
	_, err := io.WriteString(w, RenderHTML(doc))
	return err
}

// RenderHTML 把 Document 渲染成独立 HTML（内嵌样式，无外部资源）。
func RenderHTML(doc *Document) string {
	tmpl := template.Must(template.New("report").Funcs(template.FuncMap{
		"timestamp": func(t time.Time) string { return t.Format("2006-01-02 15:04:05") },
		"severityClass": func(severity string) string {
			switch severity {
			case "严重":
				return "critical"
			case "高":
				return "high"
			default:
				return "normal"
			}
		},
		"linkify":  linkifyCitations,
		"markdown": renderMarkdownToHTML,
	}).Parse(reportHTMLTemplate))

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, doc); err != nil {
		return "<p>HTML 渲染失败: " + html.EscapeString(err.Error()) + "</p>"
	}
	return buf.String()
}

// linkifyCitations 先转义纯文本，再把 [ev-x] 转成页内锚点链接。
// 只用于 Finding 等结构化安全字段，证据 Detail 不经过此函数。
func linkifyCitations(text string) template.HTML {
	escaped := html.EscapeString(text)
	linked := evMarkerPattern.ReplaceAllString(escaped, `<a href="#$1">[$1]</a>`)
	return template.HTML(linked)
}

// renderMarkdownToHTML 把 LLM 的 Markdown 正文转为 HTML；goldmark 默认不透传 raw HTML。
func renderMarkdownToHTML(content string) template.HTML {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(strings.TrimSpace(content)), &buf); err != nil {
		return template.HTML("<pre>" + html.EscapeString(content) + "</pre>")
	}
	return template.HTML(buf.String())
}
