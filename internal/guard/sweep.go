package guard

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Exposure is a credential left exposed on disk: the file and what is wrong
// with it, never its content.
type Exposure struct {
	Path, What string
}

// sweepSkip are directories the sweep never enters: caches and dependency
// trees, which hold no key of the person's and most of the files.
var sweepSkip = map[string]bool{"node_modules": true, ".cache": true, "vendor": true, ".npm": true, ".cargo": true,
	".rustup": true, ".gradle": true, ".m2": true, "Trash": true, "containers": true, ".mozilla": true, "snap": true}

// remoteCredential is a git config url line with a password or token in it.
var remoteCredential = regexp.MustCompile(`(?m)^\s*(?:push)?url\s*=\s*[a-z][a-z0-9+.-]*://[^\s/@]*:[^\s/@]+@`)

// Sweep walks each root depth levels deep for world-readable private key
// files and git repositories whose remote URL carries a credential. Symlinks
// are not followed; unreadable directories are passed over.
func Sweep(roots []string, depth int) []Exposure {
	var out []Exposure
	for _, root := range roots {
		root = filepath.Clean(root)
		base := strings.Count(root, string(filepath.Separator))
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // an unreadable entry is passed over
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					if remoteCredential.MatchString(readFile(filepath.Join(p, "config"), "")) {
						out = append(out, Exposure{filepath.Join(p, "config"), "a git remote URL with a credential"})
					}
					return filepath.SkipDir
				}
				if p != root && (sweepSkip[d.Name()] || strings.Count(p, string(filepath.Separator))-base >= depth) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() && keyName(d.Name()) {
				if info, err := d.Info(); err == nil && info.Mode().Perm()&0o004 != 0 && privateKey(p) {
					out = append(out, Exposure{p, "a world-readable private key"})
				}
			}
			return nil
		})
	}
	return out
}

// keyName: a file name keys are kept under (id_ed25519, tls.key, a .pem),
// a public key's .pub excepted. A PKCS#12 bundle is left out: from its
// name alone it cannot be told from a certificate-only one.
func keyName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pem", ".key", ".ppk":
		return true
	case ".pub":
		return false
	}
	return strings.HasPrefix(name, "id_")
}

// privateKey reports whether the file holds a private key: a PEM or PuTTY
// private key header near its start.
func privateKey(p string) bool {
	f, err := os.Open(p) //nolint:gosec // only its header is read, never reported
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	return strings.Contains(string(b), "PRIVATE KEY") || strings.HasPrefix(string(b), "PuTTY-User-Key-File")
}
