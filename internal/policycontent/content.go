package policycontent

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// TokenRule is a gitleaks rule: its id, the keywords one of which a match
// contains (a cheap prefilter), and the pattern.
type TokenRule struct {
	ID       string
	Keywords []string
	RE       *regexp.Regexp
}

func rule(id, pattern string, keywords ...string) TokenRule {
	return TokenRule{id, keywords, regexp.MustCompile(pattern)}
}

var TokenRules = []TokenRule{
	rule("github-pat", `\bghp_[0-9A-Za-z]{36}\b`, "ghp_"),
	rule("github-oauth", `\bgho_[0-9A-Za-z]{36}\b`, "gho_"),
	rule("github-app-token", `\b(?:ghu|ghs)_[0-9A-Za-z]{36}\b`, "ghu_", "ghs_"),
	rule("github-refresh-token", `\bghr_[0-9A-Za-z]{36}\b`, "ghr_"),
	rule("github-fine-grained-pat", `\bgithub_pat_[0-9A-Za-z_]{82}\b`, "github_pat_"),
	rule("gitlab-pat", `\bglpat-[0-9A-Za-z_-]{20}\b`, "glpat-"),
	rule("aws-access-token", `\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}\b`, "A3T", "AKIA", "ASIA", "ABIA", "ACCA"),
	rule("gcp-api-key", `\bAIza[0-9A-Za-z_-]{35}\b`, "AIza"),
	rule("slack-token", `\bxox[abposr]-[0-9A-Za-z-]{10,}`, "xox"),
	rule("slack-webhook-url", `hooks\.slack\.com/(?:services|workflows|triggers)/[0-9A-Za-z+/_-]{20,}`, "hooks.slack.com"),
	rule("anthropic-api-key", `\bsk-ant-[a-z]+\d{2}-[0-9A-Za-z_-]{80,}`, "sk-ant-"),
	rule("openai-api-key", `\bsk-(?:proj|svcacct|admin)-[0-9A-Za-z_-]{20,}`, "sk-proj-", "sk-svcacct-", "sk-admin-"),
	rule("npm-access-token", `\bnpm_[0-9A-Za-z]{36}\b`, "npm_"),
	rule("1password-service-account-token", `\bops_eyJ[0-9A-Za-z+/=_-]{50,}`, "ops_eyJ"),
	rule("age-secret-key", `AGE-SECRET-KEY-1[0-9A-Z]{58}`, "AGE-SECRET-KEY-1"),
	rule("private-key", `-----BEGIN[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----`, "PRIVATE KEY"),
	rule("jwt", `\beyJ[0-9A-Za-z_-]{10,}\.eyJ[0-9A-Za-z_-]{10,}\.[0-9A-Za-z_-]{10,}`, "eyJ"),
	// A password in a URL; a placeholder ($VAR, <token>, {x}, %s, ***) is none.
	rule("url-credentials", `\b[a-z][a-z0-9+.-]*://[^\s/:@'"]+:[^\s/@'"$<{%*][^\s/@'"]*@[0-9A-Za-z]`, "://"),
}

// CredentialShapes are the ids of the token rules text matches, once each:
// the shapes of a credential (a private key, a token's prefix), none for a
// public value. A gitleaks:allow marker exempts nothing here.
func CredentialShapes(text string) []string {
	var out []string
	for _, r := range TokenRules {
		if slices.ContainsFunc(r.Keywords, func(k string) bool { return strings.Contains(text, k) }) && r.RE.MatchString(text) {
			out = append(out, r.ID)
		}
	}
	return out
}

// RulesAllowed rejects credential store names and excluded source trees.
func RulesAllowed(p string, extra []string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		part = strings.ToLower(part)
		if strings.Contains(part, "credential") || strings.Contains(part, "token") || strings.Contains(part, "secret") || strings.HasPrefix(part, ".env") {
			return false
		}
		switch part {
		case ".ssh", ".aws", ".kube", "auth.json", ".netrc", ".git-credentials", "hosts.yml":
			return false
		}
	}
	home, _ := os.UserHomeDir()
	stores := append([]string(nil), extra...)
	for _, name := range []string{".omp/agent/models.yml", ".omp/agent/config.yml", ".omp/agent/agent.db", ".claude/.credentials.json", ".codex/auth.json", ".config/gh/hosts.yml", ".docker/config.json", ".netrc", ".git-credentials", ".local/state/beekeeper/codex/home/auth.json"} {
		stores = append(stores, filepath.Join(home, name))
	}
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		stores = append(stores, filepath.Join(codexHome, "auth.json"))
	}
	candidate, _ := policyFileInfo(p)
	for _, excluded := range stores {
		if strings.HasPrefix(excluded, "~/") {
			excluded = filepath.Join(home, excluded[2:])
		}
		matches, _ := filepath.Glob(excluded)
		for _, match := range matches {
			info, err := policyFileInfo(match)
			if err == nil && candidate != nil && os.SameFile(candidate, info) {
				return false
			}
		}

		if ok, _ := filepath.Match(excluded, p); ok {
			return false
		}
		rel, err := filepath.Rel(excluded, p)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return false
		}
	}
	return true
}

// policyFileInfo only inspects metadata. Rooted lookup prevents a configured
// exclusion from traversing outside its filesystem volume while still following
// aliases within that volume for the SameFile credential-store comparison.
func policyFileInfo(name string) (os.FileInfo, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(absolute) + string(filepath.Separator)
	relative, err := filepath.Rel(volume, absolute)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.Stat(relative)
}
