package report

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// 支持的格式名。
const (
	FormatMarkdown = "markdown"
	FormatHTML     = "html"
)

// Renderer 把 Document 渲染到 w。
type Renderer interface {
	Render(w io.Writer, doc *Document) error
}

// For 按格式名返回渲染器。
func For(format string) (Renderer, error) {
	switch format {
	case FormatMarkdown:
		return MarkdownRenderer{}, nil
	case FormatHTML:
		return HTMLRenderer{}, nil
	default:
		return nil, fmt.Errorf("不支持的格式 %q，支持: markdown、html", format)
	}
}

// FormatFromPath 按文件扩展名推断格式；.md→markdown，.html/.htm→html；其余返回空串。
func FormatFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md":
		return FormatMarkdown
	case ".html", ".htm":
		return FormatHTML
	default:
		return ""
	}
}

// MarkdownRenderer 实现 Markdown 渲染。
type MarkdownRenderer struct{}

// Render 实现 Renderer。
func (MarkdownRenderer) Render(w io.Writer, doc *Document) error {
	_, err := io.WriteString(w, RenderMarkdown(doc))
	return err
}
