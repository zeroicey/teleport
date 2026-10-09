// Package markdown renders report Markdown to safe HTML for the public share
// page.
//
// Security model — this is the most important control in the render path:
//
//   - Raw HTML in the source is NEVER passed through. goldmark's default
//     (unsafe=false) replaces HTML blocks and inline tags with an
//     "omitted" comment, so a report author cannot inject markup or script.
//     The old implementation used markdown-it with `html: false`, which has the
//     same effect via a different mechanism.
//   - Dangerous URL schemes (`javascript:`, `vbscript:`, `file:`, and
//     non-image `data:`) are rejected by goldmark's renderer even for autolinks.
//   - Code blocks are syntax-highlighted server-side with a curated lexer set,
//     and the result is HTML-escaped by chroma.
//
// Mermaid diagrams are deliberately NOT rendered here. Mermaid is ~5 MB, so the
// fence is emitted as `<pre class="mermaid">` and a small client script
// lazy-loads Mermaid only when such an element exists.
package markdown

import (
	"bytes"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

// RenderResult is the outcome of rendering one document.
type RenderResult struct {
	HTML string
	// HasMermaid reports whether the document contains at least one diagram, so
	// the caller can decide whether to advertise the Mermaid chunk.
	HasMermaid bool
}

// Renderer is a reusable, concurrency-safe Markdown renderer.
//
// goldmark renderers are safe for concurrent use; the chroma formatter and
// lexer registry are read-only after construction.
type Renderer struct {
	md        goldmark.Markdown
	formatter *html.Formatter
}

// New builds a renderer configured to match the previous markdown-it behaviour:
// tables and strikethrough enabled, linkification on, no raw HTML, no
// typographic substitution.
func New() *Renderer {
	r := &Renderer{
		formatter: html.New(
			// Emit CSS classes rather than inline styles, so the share page's
			// stylesheet (and dark mode) controls the colours.
			html.WithClasses(true),
			// We supply our own <pre class="code-block"> wrapper.
			html.PreventSurroundingPre(true),
		),
	}

	// Capture the stock HTML renderer's node functions so our overrides can
	// delegate the nodes we do not customise.
	//
	// This must be done on a standalone instance rather than by replacing the
	// Markdown renderer: goldmark's extensions (Table, Strikethrough, Linkify)
	// register their own node renderers onto the default renderer *after*
	// options are applied, so calling SetRenderer would silently drop them.
	// Registering our overrides as a renderer option at priority 100 instead
	// lets them win over the stock renderer's 1000 while leaving every other
	// renderer in place.
	delegate := gmhtml.NewRenderer(gmhtml.WithXHTML())
	capture := &funcCapture{funcs: map[ast.NodeKind]renderer.NodeRendererFunc{}}
	delegate.RegisterFuncs(capture)

	r.md = goldmark.New(
		goldmark.WithExtensions(
			extension.Table,
			extension.Strikethrough,
			extension.Linkify,
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
		),
		goldmark.WithRendererOptions(
			gmhtml.WithXHTML(),
			// Deliberately NOT WithUnsafe(): raw HTML stays escaped/omitted.
			// Deliberately NOT WithHardWraps(): single newlines must not become
			// <br>, matching markdown-it's `breaks: false` default.
			renderer.WithNodeRenderers(
				util.Prioritized(&nodeRenderer{r: r, inner: capture.funcs}, 100),
			),
		),
	)
	return r
}

// funcCapture records a node renderer's registered functions so an override can
// invoke the stock implementation for the cases it does not handle itself.
type funcCapture struct {
	funcs map[ast.NodeKind]renderer.NodeRendererFunc
}

func (f *funcCapture) Register(kind ast.NodeKind, fn renderer.NodeRendererFunc) {
	f.funcs[kind] = fn
}

// Render converts Markdown source to HTML.
func (r *Renderer) Render(source string) RenderResult {
	var buf bytes.Buffer
	if err := r.md.Convert([]byte(source), &buf); err != nil {
		// A render failure must not take down the page; fall back to escaped
		// plain text so the reader still sees the report.
		return RenderResult{HTML: "<pre class=\"code-block\">" + Escape(source) + "</pre>"}
	}
	out := buf.String()
	return RenderResult{HTML: out, HasMermaid: strings.Contains(out, `class="mermaid"`)}
}

// nodeRenderer overrides just the code-block and link rendering, delegating
// everything else to the stock goldmark HTML renderer.
type nodeRenderer struct {
	r     *Renderer
	inner map[ast.NodeKind]renderer.NodeRendererFunc
}

func (n *nodeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, n.renderFencedCode)
	reg.Register(ast.KindCodeBlock, n.renderIndentedCode)
	reg.Register(ast.KindLink, n.renderLink)
	reg.Register(ast.KindAutoLink, n.renderAutoLink)
}

// delegate forwards a node to the stock renderer, falling back to a plain
// continue when the stock renderer registered nothing for that kind.
func (n *nodeRenderer) delegate(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if fn, ok := n.inner[node.Kind()]; ok {
		return fn(w, source, node, entering)
	}
	return ast.WalkContinue, nil
}

// renderFencedCode emits either a Mermaid placeholder or a highlighted block.
func (n *nodeRenderer) renderFencedCode(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	block, ok := node.(*ast.FencedCodeBlock)
	if !ok {
		return ast.WalkContinue, nil
	}

	info := ""
	if block.Info != nil {
		info = string(block.Info.Text(source))
	}
	// An unlabelled fence (``` with nothing after it) yields no fields at all,
	// so the index must be guarded.
	firstWord := ""
	if fields := strings.Fields(info); len(fields) > 0 {
		firstWord = fields[0]
	}
	code := textOf(block, source)

	// Check Mermaid before language resolution: "mermaid" is not a chroma
	// lexer, so resolveLanguage would report it as unknown.
	if strings.EqualFold(firstWord, "mermaid") {
		// Mermaid reads `textContent`, so the source must be HTML-escaped here.
		// It is rendered client-side into an SVG.
		_, _ = w.WriteString(`<div class="mermaid-wrap"><pre class="mermaid">`)
		_, _ = w.WriteString(Escape(code))
		_, _ = w.WriteString(`</pre></div>`)
		return ast.WalkSkipChildren, nil
	}

	_, _ = w.WriteString(n.r.highlight(code, resolveLanguage(firstWord)))
	return ast.WalkSkipChildren, nil
}

// renderIndentedCode highlights a four-space indented block, which has no
// language. markdown-it left these unhighlighted; highlighting with the
// plaintext lexer keeps the markup uniform.
func (n *nodeRenderer) renderIndentedCode(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	block, ok := node.(*ast.CodeBlock)
	if !ok {
		return ast.WalkContinue, nil
	}
	_, _ = w.WriteString(n.r.highlight(textOf(block, source), ""))
	return ast.WalkSkipChildren, nil
}

// renderLink adds target/rel to external links, then defers to the default
// renderer so its URL-scheme filtering still applies.
func (n *nodeRenderer) renderLink(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		if link, ok := node.(*ast.Link); ok {
			dest := string(link.Destination)
			if strings.HasPrefix(dest, "http://") || strings.HasPrefix(dest, "https://") {
				link.SetAttributeString("target", "_blank")
				link.SetAttributeString("rel", "noopener noreferrer nofollow")
			}
		}
	}
	return n.delegate(w, source, node, entering)
}

// renderAutoLink defers to the stock renderer, which already drops dangerous
// schemes (`javascript:` etc.) even in safe mode. Wrapped so the override list
// stays explicit about which nodes we deliberately do not customise.
func (n *nodeRenderer) renderAutoLink(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	return n.delegate(w, source, node, entering)
}

// highlight renders one code block, always returning complete, escaped markup.
func (r *Renderer) highlight(code, language string) string {
	if language != "" {
		if lexer := lexers.Get(language); lexer != nil {
			// Coalesce merges adjacent same-type tokens, which roughly halves
			// the emitted span count.
			lexer = chroma.Coalesce(lexer)
			if iterator, err := lexer.Tokenise(nil, code); err == nil {
				var buf bytes.Buffer
				if err := r.formatter.Format(&buf, style, iterator); err == nil {
					return `<pre class="code-block" data-lang="` + Escape(language) +
						`"><code class="chroma language-` + Escape(language) + `">` +
						buf.String() + `</code></pre>`
				}
			}
		}
	}

	// No language (or highlighting failed): escape rather than emit raw source.
	label := ""
	if language != "" {
		label = ` data-lang="` + Escape(language) + `"`
	}
	return `<pre class="code-block"` + label + `><code class="chroma">` + Escape(code) + `</code></pre>`
}

// resolveLanguage maps a fence info word onto a chroma lexer name, returning ""
// when the language is unknown so the caller falls back to escaped plain text.
func resolveLanguage(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		return ""
	}
	if alias, ok := aliases[name]; ok {
		name = alias
	}
	if lexers.Get(name) == nil {
		return ""
	}
	return name
}

// aliases mirrors the friendly shorthands the previous implementation accepted.
var aliases = map[string]string{
	"sh":       "bash",
	"zsh":      "bash",
	"console":  "bash",
	"ts":       "typescript",
	"js":       "javascript",
	"jsx":      "javascript",
	"tsx":      "typescript",
	"py":       "python",
	"yml":      "yaml",
	"ps1":      "powershell",
	"html":     "xml",
	"svg":      "xml",
	"md":       "markdown",
	"text":     "plaintext",
	"txt":      "plaintext",
	"conf":     "ini",
	"toml":     "ini",
	"cxx":      "cpp",
	"cs":       "csharp",
	"golang":   "go",
	"rs":       "rust",
	"docker":   "dockerfile",
	"k8s":      "yaml",
	"shell":    "bash",
	"terminal": "bash",
}

// style is an empty chroma style: colours come entirely from our stylesheet,
// which is what makes dark mode possible without regenerating CSS per request.
var style = chroma.MustNewStyle("teleport", chroma.StyleEntries{})

// textOf concatenates the raw source lines of a code block.
func textOf(node ast.Node, source []byte) string {
	lines := node.Lines()
	var sb strings.Builder
	for i := 0; i < lines.Len(); i++ {
		segment := lines.At(i)
		sb.Write(segment.Value(source))
	}
	return sb.String()
}

// Escape HTML-escapes a string for safe interpolation into an HTML context.
func Escape(value string) string {
	var sb strings.Builder
	sb.Grow(len(value) + 16)
	for _, r := range value {
		switch r {
		case '&':
			sb.WriteString("&amp;")
		case '<':
			sb.WriteString("&lt;")
		case '>':
			sb.WriteString("&gt;")
		case '"':
			sb.WriteString("&quot;")
		case '\'':
			sb.WriteString("&#39;")
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
