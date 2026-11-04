package secret

import (
	"fmt"
	"os"
)

// SecretKeysPlan describes a write using public metadata and key names only.
type SecretKeysPlan struct {
	File       string   `json:"file"`
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	Recipients []string `json:"recipients"`
	Keys       []string `json:"keys"`
}

// PlanSecretKeys checks an existing Secret and its creation rule without
// decrypting it, reading an identity, invoking a tool or writing any file.
// It does not prove access to a recipient's private identity.
func PlanSecretKeys(file string, keys []string, replace bool) (*SecretKeysPlan, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // the configured target, never decrypted
	if err != nil {
		return nil, fmt.Errorf("target Secret: %w", err)
	}
	doc, err := parseDocument(raw)
	if err != nil {
		// A YAML parse error can quote a line containing a value.
		return nil, fmt.Errorf("%s: target Secret is no YAML mapping", file)
	}
	if kind, _ := doc.get("kind"); kind != secretKind {
		return nil, fmt.Errorf("%s holds no Kubernetes Secret", file)
	}
	name, _ := doc.get("metadata.name")
	namespace, _ := doc.get("metadata.namespace")
	if name == "" {
		return nil, fmt.Errorf("%s: target Secret has no metadata.name", file)
	}
	if !sopsMetadata(raw) {
		if _, err := skeleton(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	}
	cfg, rel, err := sopsTarget(file)
	if err != nil {
		return nil, err
	}
	rule, err := ruleFor(cfg, rel)
	if err != nil {
		return nil, err
	}
	if rule == nil {
		return nil, fmt.Errorf("%s: no creation rule of %s applies to it", file, cfg)
	}
	recipients := rule.recipients()
	if len(recipients) == 0 {
		return nil, fmt.Errorf("%s: the creation rule in %s names no valid age recipient", file, cfg)
	}
	var paths []string
	for _, key := range keys {
		path, err := place(doc, file, key)
		if err != nil {
			return nil, err
		}
		inData := doc.node("data."+key) != nil
		inString := doc.node(path) != nil
		if (inString || inData) && !replace {
			return nil, fmt.Errorf("%s: the Secret holds %s already: replacing it takes --replace", file, key)
		}
		paths = append(paths, path)
	}
	return &SecretKeysPlan{File: file, Name: name, Namespace: namespace, Recipients: recipients, Keys: paths}, nil
}
