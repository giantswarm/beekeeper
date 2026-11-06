package guard

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run against a real layout engine: DOM mocks do not implement the box
// semantics of checkVisibility or display:contents. Set CHROME_BIN when
// Chromium is not on PATH.
func TestUploadTargetBrowser(t *testing.T) {
	browser := os.Getenv("CHROME_BIN")
	if browser == "" {
		for _, name := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if path, err := exec.LookPath(name); err == nil {
				browser = path
				break
			}
		}
	}
	if browser == "" {
		t.Skip("Chromium unavailable; set CHROME_BIN to run layout regression tests")
	}
	fixture, err := os.ReadFile("testdata/upload-comment-box.html")
	if err != nil {
		t.Fatal(err)
	}
	const (
		commentsForm = `form[action="/org/repo/comments"]`
		commentBox   = "#comment-box"
	)
	cases := []struct {
		name, html, scope, setup string
		marked                   bool
	}{
		{name: "issue form", html: `<form action="/org/repo/comments"><input type="file"></form>`, scope: commentsForm, marked: true},
		{name: "pull request sticky token", html: `<form action="/org/repo/comments?sticky=secret"><input type="file"></form>`, scope: `form[action="/org/repo/comments?sticky=[BLOCKED: Cookie/query string data]"]`, marked: true},
		{name: "absolute destination", html: `<form action="/org/repo/comments?sticky=x"><input type="file"></form>`, scope: `form[action='https://example.invalid/org/repo/comments?sticky=redacted']`, marked: true},
		{name: "other scheme", html: `<form action="http://example.invalid/org/repo/comments"><input type="file"></form>`, scope: commentsForm},
		{name: "other host", html: `<form action="https://other.invalid/org/repo/comments?sticky=x"><input type="file"></form>`, scope: commentsForm},
		{name: "other path", html: `<form action="/org/repo/reviews?sticky=x"><input type="file"></form>`, scope: commentsForm},
		{name: "ambiguous destination", html: `<form action="/comments?a"><input type="file"></form><form action="/comments?b"><input type="file"></form>`, scope: `form[action="/comments"]`},
		{name: "boxless comment wrapper", html: string(fixture), scope: commentBox, marked: true},
		{name: "hidden comment ancestor", html: string(fixture), scope: commentBox, setup: `document.querySelector('.editor').style.display='none';`},
		{name: "invisible comment ancestor", html: string(fixture), scope: commentBox, setup: `document.querySelector('.editor').style.visibility='hidden';`},
		{name: "hidden attribute", html: string(fixture), scope: commentBox, setup: `document.querySelector('#comment-box').hidden=true;`},
		{name: "inert ancestor", html: string(fixture), scope: commentBox, setup: `document.querySelector('#comment-box').inert=true;`},
		{name: "disabled input", html: string(fixture), scope: commentBox, setup: `document.querySelector('input').disabled=true;`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Supply an explicit base so relative and absolute destinations resolve alike.
			q, _ := json.Marshal(tc.scope)
			html := `<base href="https://example.invalid/">` + tc.html + `<script>` + tc.setup +
				`const scope = ` + string(q) + `; const result = ` + UploadTargetScript + `;</script>`
			html += `<script>document.body.setAttribute('data-marked',String(result.startsWith('` + uploadMarked + `')));document.body.setAttribute('data-labels',String(document.querySelectorAll('input[aria-label="` + UploadTargetLabel + `"]').length));</script>`
			path := filepath.Join(t.TempDir(), "upload.html")
			if err := os.WriteFile(path, []byte(html), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, browser, "--headless", "--no-sandbox", "--disable-gpu", "--dump-dom", "file://"+path).Output() //nolint:gosec // the machine's browser and the test's own file
			if err != nil {
				t.Fatalf("Chromium: %v", err)
			}
			want := `data-marked="false" data-labels="0"`
			if tc.marked {
				want = `data-marked="true" data-labels="1"`
			}
			if !strings.Contains(string(out), want) {
				t.Fatalf("want %s in rendered DOM: %s", want, out)
			}
		})
	}
}
