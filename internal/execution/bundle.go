package execution

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/giantswarm/beekeeper/internal/policycontent"
)

const BundleSchema = 1
const bundleRulesKind = "rules"
const bundleSkillsKind = "skills"
const bundlePluginsKind = "plugins"

// maxBundleBytes leaves a bundle room inside the one wire request that carries it.
const maxBundleBytes = MaxWire - 64<<10
const maxArtifactBytes = 256 << 10

// BundleSource explicitly approves one file. Directories are never harvested.
// Rules use rules/<name>/AGENTS.md or CLAUDE.md; skills use skills/<name>/... ;
// plugins use plugins/<name>/skills/<skill>/... and plugins/<name>/commands/<file>.md.
type BundleSource struct {
	Source   string `yaml:"source" json:"source"`
	Artifact string `yaml:"artifact" json:"artifact"`
}
type BundleArtifact struct {
	BundleSource
	Digest  string `json:"digest"`
	Content []byte `json:"content"`
}

// BundleCapability declares a dependency, never connector configuration or credentials.
type BundleCapability struct {
	Name     string `yaml:"name" json:"name"`
	Required bool   `yaml:"required" json:"required"`
}
type CoordinatorBundle struct {
	Schema       int                `json:"schema"`
	Artifacts    []BundleArtifact   `json:"artifacts"`
	Settings     map[string]string  `json:"settings,omitempty"`
	Capabilities []BundleCapability `json:"capabilities,omitempty"`
}

func (b CoordinatorBundle) Digest() string { return Digest(mustJSON(b)) }

// BuildCoordinatorBundle captures only explicitly approved files below root.
// secretFiles supplies the coordinator's configured secret-store exclusions.
func BuildCoordinatorBundle(root string, sources []BundleSource, settings map[string]string, capabilities []BundleCapability, secretFiles []string) (CoordinatorBundle, error) {
	b := CoordinatorBundle{Schema: BundleSchema, Settings: settings, Capabilities: append([]BundleCapability(nil), capabilities...)}
	if len(sources) == 0 || len(sources) > 256 {
		return b, errors.New("bundle source count exceeds bounds")
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(root) || real != filepath.Clean(root) {
		return b, errors.New("bundle source root must be absolute without symlinks")
	}
	for _, src := range sources {
		file := filepath.Join(root, filepath.FromSlash(src.Source))
		resolved, err := filepath.EvalSymlinks(file)
		if !safeRelative(src.Source) || err != nil || resolved != file || !within(root, resolved) || !policycontent.RulesAllowed(file, secretFiles) {
			return b, errors.New("bundle source is outside approved paths or names a credential store")
		}
		data, err := readRegular(file, maxArtifactBytes)
		if err != nil {
			return b, err
		}
		b.Artifacts = append(b.Artifacts, BundleArtifact{BundleSource: src, Digest: Digest(data), Content: data})
	}
	sort.Slice(b.Artifacts, func(i, j int) bool { return b.Artifacts[i].Artifact < b.Artifacts[j].Artifact })
	sort.Slice(b.Capabilities, func(i, j int) bool { return b.Capabilities[i].Name < b.Capabilities[j].Name })
	return b, b.Validate()
}

func safeRelative(p string) bool {
	return p != "." && p != "" && path.Clean(p) == p && !strings.HasPrefix(p, "../") && !strings.HasPrefix(p, "/") && !strings.ContainsAny(p, "\\\x00\r\n")
}

var capabilityName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}:[a-zA-Z0-9_-]{1,64}$`)

func (b CoordinatorBundle) Validate() error {
	if b.Schema != BundleSchema {
		return fmt.Errorf("unsupported coordinator bundle schema %d", b.Schema)
	}
	if len(b.Artifacts) == 0 || len(b.Artifacts) > 256 || len(mustJSON(b)) > maxBundleBytes {
		return errors.New("coordinator bundle exceeds bounds or has no artifacts")
	}
	previous := ""
	rulesBytes := 0
	skills, plugins := map[string]bool{}, map[string]bool{}
	for _, a := range b.Artifacts {
		parts := strings.Split(a.Artifact, "/")
		if !safeRelative(a.Source) || !safeRelative(a.Artifact) || (a.Artifact <= previous || previous != "" && strings.HasPrefix(a.Artifact, previous+"/")) || len(parts) < 3 || (parts[0] != bundleRulesKind && parts[0] != bundleSkillsKind && parts[0] != bundlePluginsKind) {
			return errors.New("invalid or duplicate bundle artifact path")
		}
		if parts[0] == bundlePluginsKind {
			if !pluginDocument(parts) {
				return fmt.Errorf("unsupported plugin artifact %s: a plugin travels as its skills and commands only", a.Artifact)
			}
			plugins[parts[1]] = true
		}
		if parts[0] == bundleRulesKind && (len(parts) != 3 || (parts[2] != "AGENTS.md" && parts[2] != claudeRulesFile)) {
			return errors.New("unsupported rules artifact")
		}
		if !policycontent.RulesAllowed("/bundle/"+a.Source, nil) || !policycontent.RulesAllowed("/bundle/"+a.Artifact, nil) || len(a.Content) == 0 || len(a.Content) > maxArtifactBytes || !utf8.Valid(a.Content) || strings.IndexByte(string(a.Content), 0) >= 0 || len(policycontent.CredentialShapes(string(a.Content))) > 0 || Digest(a.Content) != a.Digest {
			return fmt.Errorf("invalid or credential-shaped bundle artifact %s", a.Artifact)
		}
		if skill, entry, ok := bundleSkill(parts); ok {
			skills[skill] = skills[skill] || entry
		}
		if parts[0] == bundleRulesKind {
			rulesBytes += len(a.Content)
			if rulesBytes > maxArtifactBytes {
				return errors.New("combined coordinator rules exceed bounds")
			}
		}
		previous = a.Artifact
	}
	for name, present := range skills {
		if !present {
			return fmt.Errorf("missing SKILL.md for skill %s", name)
		}
	}
	for name, value := range b.Settings {
		if name != "model_reasoning_effort" || (value != "low" && value != "medium" && value != "high" && value != "xhigh") {
			return fmt.Errorf("unsupported required Codex setting %s", name)
		}
	}
	previous = ""
	for _, c := range b.Capabilities {
		if !capabilityName.MatchString(c.Name) || c.Name <= previous {
			return errors.New("invalid or duplicate capability declaration")
		}
		previous = c.Name
		if plugin, ok := strings.CutPrefix(c.Name, "plugin:"); c.Required && (!ok || !plugins[plugin]) {
			return fmt.Errorf("unsupported required capability %s", c.Name)
		}
	}
	return nil
}

// pluginDocument admits the parts of a plugin that are documents: its skills and
// its commands. Hooks, MCP servers and anything else that would execute on a host
// have no representation, so the host's guards stay the only ones.
func pluginDocument(parts []string) bool {
	switch {
	case len(parts) >= 5 && parts[2] == bundleSkillsKind:
		return true
	case len(parts) == 4 && parts[2] == "commands":
		return strings.HasSuffix(parts[3], ".md")
	}
	return false
}

// bundleSkill names the skill an artifact belongs to and says whether it is that
// skill's entry point; ok is false for what is no skill.
func bundleSkill(parts []string) (skill string, entry, ok bool) {
	switch {
	case parts[0] == bundleSkillsKind:
		return parts[1], len(parts) == 3 && parts[2] == "SKILL.md", true
	case parts[0] == bundlePluginsKind && parts[2] == bundleSkillsKind:
		return parts[1] + "-" + parts[3], len(parts) == 5 && parts[4] == "SKILL.md", true
	}
	return "", false, false
}

// codexSkillFile is where codex reads a skill artifact from in its home, a
// plugin's under skills/<plugin>-<skill>; ok is false for anything else, which
// goes into its rules.
func codexSkillFile(artifact string) (string, bool) {
	parts := strings.Split(artifact, "/")
	skill, _, ok := bundleSkill(parts)
	if !ok {
		return "", false
	}
	depth := 2
	if parts[0] == bundlePluginsKind {
		depth = 4
	}
	return path.Join(append([]string{bundleSkillsKind, skill}, parts[depth:]...)...), true
}
