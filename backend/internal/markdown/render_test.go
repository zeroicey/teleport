package markdown

import (
	"strings"
	"testing"
)

// TestRawHTMLIsNeverPassedThrough is the single most important security
// assertion in the render path: a report author must not be able to inject
// markup. Both block-level and inline raw HTML are covered.
func TestRawHTMLIsNeverPassedThrough(t *testing.T) {
	r := New()

	cases := []struct {
		name   string
		source string
		banned []string
	}{
		{
			name:   "script block",
			source: "<script>alert(1)</script>",
			banned: []string{"<script>", "alert(1)"},
		},
		{
			name:   "inline onerror attribute",
			source: `hello <img src=x onerror="alert(1)"> world`,
			banned: []string{"<img", "onerror"},
		},
		{
			name:   "inline iframe",
			source: `before <iframe src="https://evil.example"></iframe> after`,
			banned: []string{"<iframe"},
		},
		{
			name:   "html comment with markup",
			source: "<!-- <script>alert(1)</script> -->",
			banned: []string{"<script>"},
		},
		{
			name:   "svg onload",
			source: `<svg onload="alert(1)"></svg>`,
			banned: []string{"<svg", "onload"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Render(tc.source).HTML
			for _, bad := range tc.banned {
				if strings.Contains(got, bad) {
					t.Errorf("raw HTML leaked: output contains %q\n%s", bad, got)
				}
			}
		})
	}
}

// TestDangerousURLSchemesAreRejected covers both explicit links and
// autolinked bare URLs.
//
// The assertion is deliberately on href values rather than on raw substring
// presence: goldmark neutralises a dangerous autolink by emptying the href and
// leaving the (escaped, inert) text visible, so searching the whole document for
// "javascript:" would match harmless display text.
func TestDangerousURLSchemesAreRejected(t *testing.T) {
	r := New()

	cases := []struct {
		name   string
		source string
	}{
		{"explicit javascript link", `[click](javascript:alert(1))`},
		{"explicit vbscript link", `[click](vbscript:msgbox(1))`},
		{"explicit file link", `[click](file:///etc/passwd)`},
		{"autolinked javascript", `<javascript:alert(1)>`},
		{"data url non image", `[click](data:text/html;base64,PHNjcmlwdD4=)`},
	}

	dangerous := []string{"javascript:", "vbscript:", "file://", "data:text/html", "data:application"}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Render(tc.source).HTML
			for _, href := range hrefsOf(got) {
				for _, bad := range dangerous {
					if strings.HasPrefix(strings.ToLower(href), bad) {
						t.Errorf("dangerous href %q leaked:\n%s", href, got)
					}
				}
			}
		})
	}
}

// hrefsOf extracts every href="..." attribute value from a rendered fragment.
func hrefsOf(html string) []string {
	var out []string
	for _, part := range strings.Split(html, `href="`) {
		if idx := strings.Index(part, `"`); idx >= 0 {
			out = append(out, part[:idx])
		}
	}
	return out
}

// TestExtensionsWork guards against the renderer-replacement regression where
// goldmark extensions register onto the default renderer and get dropped by a
// naive SetRenderer call.
func TestExtensionsWork(t *testing.T) {
	r := New()

	t.Run("table", func(t *testing.T) {
		got := r.Render("| a | b |\n| - | - |\n| 1 | 2 |\n").HTML
		for _, want := range []string{"<table>", "<th>", "<td>"} {
			if !strings.Contains(got, want) {
				t.Errorf("table extension not applied, missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("strikethrough", func(t *testing.T) {
		got := r.Render("~~gone~~").HTML
		if !strings.Contains(got, "<del>") {
			t.Errorf("strikethrough extension not applied:\n%s", got)
		}
	})

	t.Run("linkify", func(t *testing.T) {
		got := r.Render("see https://example.com/x for details").HTML
		if !strings.Contains(got, `href="https://example.com/x"`) {
			t.Errorf("linkify extension not applied:\n%s", got)
		}
	})

	t.Run("heading ids", func(t *testing.T) {
		got := r.Render("## Section title").HTML
		if !strings.Contains(got, `id="section-title"`) {
			t.Errorf("auto heading id not applied:\n%s", got)
		}
	})

	t.Run("hard wraps disabled", func(t *testing.T) {
		got := r.Render("line one\nline two").HTML
		if strings.Contains(got, "<br") {
			t.Errorf("single newline became a line break:\n%s", got)
		}
	})
}

// TestCodeBlocks checks that fenced code is highlighted into complete, escaped
// markup and that the language label survives.
func TestCodeBlocks(t *testing.T) {
	r := New()

	t.Run("known language is highlighted", func(t *testing.T) {
		got := r.Render("```go\npackage main\n```\n").HTML
		if !strings.Contains(got, `<pre class="code-block" data-lang="go">`) {
			t.Errorf("missing wrapper with data-lang:\n%s", got)
		}
		if !strings.Contains(got, `<code class="chroma language-go">`) {
			t.Errorf("missing code element:\n%s", got)
		}
		if !strings.Contains(got, `<span class="kn">package</span>`) {
			t.Errorf("expected keyword span from chroma:\n%s", got)
		}
	})

	t.Run("alias resolves", func(t *testing.T) {
		got := r.Render("```ts\nconst x: number = 1;\n```\n").HTML
		if !strings.Contains(got, `data-lang="typescript"`) {
			t.Errorf("ts alias did not resolve to typescript:\n%s", got)
		}
	})

	t.Run("unknown language falls back to escaped text", func(t *testing.T) {
		got := r.Render("```notalanguage\n<b>&\"'</b>\n```\n").HTML
		if strings.Contains(got, "<b>") {
			t.Errorf("code content was not escaped:\n%s", got)
		}
		for _, want := range []string{"&lt;b&gt;", "&amp;", "&quot;", "&#39;"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing escape %q:\n%s", want, got)
			}
		}
	})

	t.Run("unlabelled fence is escaped", func(t *testing.T) {
		got := r.Render("```\n<script>x</script>\n```\n").HTML
		if strings.Contains(got, "<script>") {
			t.Errorf("unlabelled code content was not escaped:\n%s", got)
		}
	})
}

// TestMermaidFence verifies the Mermaid placeholder contract that the client
// script depends on: a `pre.mermaid` with escaped source and a wrapping div.
func TestMermaidFence(t *testing.T) {
	r := New()
	got := r.Render("```mermaid\ngraph TD;\n  A-->B;\n```\n").HTML

	if !strings.Contains(got, `<div class="mermaid-wrap"><pre class="mermaid">`) {
		t.Errorf("missing mermaid wrapper:\n%s", got)
	}
	if !strings.Contains(got, "graph TD;") {
		t.Errorf("mermaid source missing:\n%s", got)
	}

	result := r.Render("```mermaid\ngraph TD;\n  A-->B;\n```\n")
	if !result.HasMermaid {
		t.Error("HasMermaid should be true for a mermaid document")
	}
	if plain := r.Render("just text\n"); plain.HasMermaid {
		t.Error("HasMermaid should be false for a plain document")
	}
}

// TestMermaidSourceIsEscaped guards the XSS path where a diagram label tries to
// close the pre element; Mermaid reads textContent, so escaping is required.
func TestMermaidSourceIsEscaped(t *testing.T) {
	r := New()
	got := r.Render("```mermaid\ngraph TD;\n  A[\"</pre><script>alert(1)</script>\"]-->B;\n```\n").HTML
	if strings.Contains(got, "</pre><script>") {
		t.Errorf("mermaid source broke out of the pre element:\n%s", got)
	}
}

// TestExternalLinksGetNoopener verifies the target/rel hardening while leaving
// relative links untouched.
func TestExternalLinksGetNoopener(t *testing.T) {
	r := New()

	external := r.Render("[x](https://example.com)").HTML
	for _, want := range []string{`target="_blank"`, "noopener", "noreferrer", "nofollow"} {
		if !strings.Contains(external, want) {
			t.Errorf("external link missing %q:\n%s", want, external)
		}
	}

	internal := r.Render("[x](/dashboard/reports)").HTML
	if strings.Contains(internal, "_blank") {
		t.Errorf("relative link should not open in a new tab:\n%s", internal)
	}
}

// TestEscape covers the helper used for every interpolated value.
func TestEscape(t *testing.T) {
	got := Escape(`<a href="x" data-y='z'>&</a>`)
	want := `&lt;a href=&quot;x&quot; data-y=&#39;z&#39;&gt;&amp;&lt;/a&gt;`
	if got != want {
		t.Errorf("Escape mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestRenderIsConcurrencySafe exercises the renderer from multiple goroutines,
// because a single instance is shared by all requests.
func TestRenderIsConcurrencySafe(t *testing.T) {
	r := New()
	sources := []string{
		"# Title\n\n```go\nfunc main() {}\n```\n",
		"| a | b |\n| - | - |\n| 1 | 2 |\n",
		"```mermaid\ngraph TD;\n A-->B;\n```\n",
		"~~done~~ and https://example.com",
	}
	done := make(chan string, len(sources)*8)
	for i := 0; i < 8; i++ {
		for _, src := range sources {
			go func(s string) { done <- r.Render(s).HTML }(src)
		}
	}
	for i := 0; i < len(sources)*8; i++ {
		if got := <-done; got == "" {
			t.Fatal("empty render result under concurrency")
		}
	}
}
