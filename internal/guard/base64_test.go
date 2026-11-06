package guard

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ageRule is the token rule of an age identity.
const ageRule = "age-secret-key"

// ageIdentity is a scratch age identity file built at run time, so no
// identity is written in the source; a failure never prints it.
func ageIdentity() (file, key string) {
	key = "AGE-SECRET-KEY-1" + strings.Repeat("Q7XZ", 14) + "K2"
	return "# created: 2026-01-01T00:00:00Z\n# public key: age1" + strings.Repeat("q", 58) + "\n" + key + "\n", key
}

// wrap breaks s into lines of n characters, each after indent.
func wrap(s string, n int, indent string) string {
	var b strings.Builder
	for len(s) > n {
		b.WriteString(indent + s[:n] + "\n")
		s = s[n:]
	}
	b.WriteString(indent + s)
	return b.String()
}

func TestRedactFindsAnAgeIdentityInsideBase64(t *testing.T) {
	file, key := ageIdentity()
	std := base64.StdEncoding.EncodeToString([]byte(file))
	twice := base64.StdEncoding.EncodeToString([]byte("kind: Config\nclient-key-data: " + std + "\n"))
	for name, text := range map[string]string{
		"Secret manifest": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: sops-age\ndata:\n  keys.txt: " + std + "\ntype: Opaque\n",
		"Secret as JSON":  `{"apiVersion":"v1","kind":"Secret","data":{"keys.txt":"` + std + `"}}`,
		"wrapped output":  "$ base64 keys.txt\n" + wrap(std, 76, "") + "\n$ echo done\n",
		"wrapped value":   "data:\n  keys.txt: |\n" + wrap(std, 64, "    ") + "\n  other: dmFsdWU=\n",
		"inline":          "the attachment is " + std + ", see above",
		"URL alphabet":    "token=" + base64.RawURLEncoding.EncodeToString([]byte("~~~"+file)),
		"encoded twice":   "data:\n  config: " + twice + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, found := (&Index{}).Redact(text)
			if strings.Contains(out, key) {
				t.Fatal("the identity survived in plain text")
			}
			for _, b64 := range []string{std[8:40], twice[8:40]} {
				if strings.Contains(out, b64) {
					t.Fatal("the encoded identity survived")
				}
			}
			if !strings.Contains(out, "[redacted: "+ageRule+"]") {
				t.Fatal("no marker naming the age-secret-key rule")
			}
			if len(found) != 1 || found[0].Rule != ageRule || found[0].Count != 1 {
				t.Fatalf("findings = %+v", found)
			}
		})
	}
}

func TestRedactKeepsWhatSurroundsAnEncodedSecret(t *testing.T) {
	file, _ := ageIdentity()
	std := base64.StdEncoding.EncodeToString([]byte(file))
	text := "data:\n  keys.txt: " + std + "\n  other: " + base64.StdEncoding.EncodeToString([]byte("an ordinary value, long enough")) + "\n"
	out, _ := (&Index{}).Redact(text)
	want := "data:\n  keys.txt: [redacted: " + ageRule + "]\n  other: " + base64.StdEncoding.EncodeToString([]byte("an ordinary value, long enough")) + "\n"
	if out != want {
		t.Fatal("Redact changed more than the encoded identity")
	}
}

func TestRedactLeavesOrdinaryBase64Alone(t *testing.T) {
	for _, text := range []string{
		"data:\n  config: " + base64.StdEncoding.EncodeToString([]byte("listen: 0.0.0.0:8080\nlog: debug\nreplicas: 3\n")) + "\n",
		wrap(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("an ordinary file of no secret. ", 12))), 76, ""),
		"/home/someone/projects/giantswarm/beekeeper/internal/guard/base64.go",
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if out, found := (&Index{}).Redact(text); out != text || found != nil {
			t.Fatalf("Redact changed ordinary text: %q %+v", out, found)
		}
	}
	file, _ := ageIdentity()
	allowedLine := "fixture " + base64.StdEncoding.EncodeToString([]byte(file)) + " // " + allowMarker
	if out, found := (&Index{}).Redact(allowedLine); out != allowedLine || found != nil {
		t.Fatalf("a %s line was redacted", allowMarker)
	}
}

func TestRedactFindsAnIndexedValueInsideAnEncodedFile(t *testing.T) {
	ix := plantedIndex(t)
	env := base64.StdEncoding.EncodeToString([]byte("USER=someone\nAPI_TOKEN=" + planted + "\nREGION=eu\n"))
	out, found := ix.Redact("data:\n  app.env: " + env + "\n")
	if strings.Contains(out, env) || !strings.Contains(out, "[redacted: "+plantedRef+"]") {
		t.Fatalf("the encoded file survived: %q", out)
	}
	if len(found) != 1 || found[0].Ref != plantedRef {
		t.Fatalf("findings = %+v", found)
	}
}

func TestPostToolUseRedactsAnEncodedAgeIdentity(t *testing.T) {
	file, key := ageIdentity()
	std := base64.StdEncoding.EncodeToString([]byte(file))
	manifest := "apiVersion: v1\nkind: Secret\ndata:\n  keys.txt: " + std + "\n"
	resp, _ := json.Marshal(struct {
		Stdout string `json:"stdout"`
	}{manifest + "$ base64 keys.txt\n" + wrap(std, 76, "")})
	in, _ := json.Marshal(ToolResult{Event: PostToolUseEvent, Tool: bashTool, Response: resp})
	out, _, found := PostToolUse(in, &Index{})
	if out == nil {
		t.Fatal("no answer for a result carrying an encoded identity")
	}
	if strings.Contains(string(out), key) || strings.Contains(string(out), std[8:40]) {
		t.Fatal("the answer carries the identity")
	}
	if len(found) != 1 || found[0].Rule != ageRule || found[0].Count != 2 {
		t.Fatalf("findings = %+v", found)
	}
}

func TestSweepTranscriptsNamesTheFilesOfAnEncodedIdentity(t *testing.T) {
	file, key := ageIdentity()
	std := base64.StdEncoding.EncodeToString([]byte(file))
	dir := t.TempDir()
	leaky := filepath.Join(dir, "-home-me-project", "abc.jsonl")
	clean := filepath.Join(dir, "-home-me-project", "def.jsonl")
	if err := os.MkdirAll(filepath.Dir(leaky), 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "toolUseResult": "data:\n  keys.txt: " + std})
	for p, content := range map[string]string{leaky: string(line) + "\n", clean: `{"type":"assistant","message":"clean"}` + "\n"} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := SweepTranscripts(dir, &Index{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Leaks) != 1 || rep.Leaks[0].Rule != ageRule || rep.Leaks[0].Files != 1 ||
		len(rep.Leaks[0].Paths) != 1 || rep.Leaks[0].Paths[0] != leaky {
		t.Fatalf("leaks = %+v", rep.Leaks)
	}
	js, _ := json.Marshal(rep)
	if strings.Contains(string(js), key) || strings.Contains(string(js), std[8:40]) {
		t.Fatal("the report carries the identity")
	}
}

func TestAuditEncodedIndexedValueAllowMarker(t *testing.T) {
	ix := plantedIndex(t)
	file, _ := ageIdentity()
	document := "MODE=test\nTOKEN=" + planted + "\n" + file
	encoded := base64.StdEncoding.EncodeToString([]byte(document))
	for name, value := range map[string]string{
		"document":        encoded,
		"nested document": base64.StdEncoding.EncodeToString([]byte("attachment: " + encoded + "\n")),
		"decoded marker":  base64.StdEncoding.EncodeToString([]byte("TOKEN=" + planted + " # " + allowMarker + "\n")),
	} {
		t.Run(name, func(t *testing.T) {
			text := "config: " + value + " # " + allowMarker
			out, found := ix.Redact(text)
			if out != "config: [redacted: "+plantedRef+"] # "+allowMarker {
				t.Fatal("encoded indexed value was not replaced with its reference")
			}
			if len(found) != 1 || found[0].Ref != plantedRef || found[0].Rule != "" || found[0].Count != 1 {
				t.Fatalf("findings = %+v", found)
			}
		})
	}
}

func TestRedactAllowsEncodedPatternsWithAllowMarker(t *testing.T) {
	file, _ := ageIdentity()
	encoded := base64.StdEncoding.EncodeToString([]byte(file))
	for _, value := range []string{
		encoded,
		base64.StdEncoding.EncodeToString([]byte("attachment: " + encoded + "\n")),
		base64.StdEncoding.EncodeToString([]byte("ordinary configuration document\n")),
	} {
		text := "config: " + value + " # " + allowMarker
		if out, found := plantedIndex(t).Redact(text); out != text || found != nil {
			t.Fatal("allow marker did not preserve an encoded document without indexed values")
		}
	}
}

func TestRedactSkipsImagesAndScansSurroundingText(t *testing.T) {
	file, key := ageIdentity()
	encoded := base64.StdEncoding.EncodeToString([]byte(file))
	for name, image := range map[string]string{
		"PNG":            base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("payload", 20))),
		"JPEG":           base64.RawURLEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0" + strings.Repeat("payload", 20))),
		"wrapped PNG":    wrap(base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"+strings.Repeat("payload", 20))), 76, "  "),
		"image data URL": "data:image/gif;base64," + encoded,
	} {
		t.Run(name, func(t *testing.T) {
			// Include a literal token pattern inside the encoded image run.
			image = strings.TrimRight(image, "=") + "ghp_" + strings.Repeat("A", 36)
			text := key + ` "` + image + `" ` + encoded
			out, found := (&Index{}).Redact(text)
			want := "[redacted: " + ageRule + "] \"" + image + "\" [redacted: " + ageRule + "]"
			if out != want || len(found) != 1 || found[0].Rule != ageRule || found[0].Count != 2 {
				t.Fatal("image changed or surrounding secrets were not redacted")
			}
		})
	}
}

func BenchmarkRedactPNG4MiB(b *testing.B) {
	// Incompressible pixels model a 4 MiB screenshot as a real PNG.
	pixels := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	if _, err := rand.Read(pixels.Pix); err != nil {
		b.Fatal(err)
	}
	var payload bytes.Buffer
	if err := png.Encode(&payload, pixels); err != nil {
		b.Fatal(err)
	}
	image := base64.StdEncoding.EncodeToString(payload.Bytes())
	ix := &Index{}
	b.SetBytes(int64(payload.Len()))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if out, found := ix.Redact(image); out != image || len(found) != 0 {
			b.Fatal("image was redacted")
		}
	}
}
