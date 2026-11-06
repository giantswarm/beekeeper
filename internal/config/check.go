package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

const configFindingFormat = "config: %v"

// Check validates the selected file without opening state or resolving secrets.
// The schema comes from Config, including inline fields and dynamic map keys.
// Only explicitly configured local input files are inspected; output paths,
// globs, remote paths and defaults need not exist yet.
func Check(path string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return []string{fmt.Sprintf(configFindingFormat, err)}, nil
	}
	var findings []string
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: yamlMappingTag}}
	}
	// Resolve YAML precedence before inspecting types or file references.
	root := effectiveNode(doc.Content[0], "", map[*yaml.Node]bool{}, &findings)
	var invalid []string
	root = checkNode(root, reflect.TypeFor[Config](), "", &findings, &invalid)
	c := &Config{}
	if err := root.Decode(c); err != nil {
		return append(findings, fmt.Sprintf(configFindingFormat, err)), nil
	}
	if err := c.defaults(); err != nil {
		return append(findings, fmt.Sprintf(configFindingFormat, err)), nil
	}
	// Invalid leaves were removed before decoding. Do not report derivative
	// value-rule failures of a section whose input could not be decoded.
	report := func(err error) {
		key, _, _ := strings.Cut(err.Error(), ":")
		key, _, _ = strings.Cut(key, " ")
		for _, bad := range invalid {
			if key == bad || strings.HasPrefix(key, bad+".") || strings.HasPrefix(key, bad+"[") ||
				strings.HasPrefix(bad, key+".") || strings.HasPrefix(bad, key+"[") {
				return
			}
		}
		findings = append(findings, err.Error())
	}
	c.validateEach(report)
	for _, message := range c.Incomplete() {
		report(fmt.Errorf("%s", message))
	}
	return findings, nil
}

// effectiveNode expands aliases and merge mappings. Explicit keys win over
// inherited keys, and the first mapping in a merge sequence wins over later
// ones. Overridden values are never visited, including their file references.
func effectiveNode(n *yaml.Node, key string, active map[*yaml.Node]bool, findings *[]string) *yaml.Node {
	if active[n] {
		*findings = append(*findings, configKey("", key)+": recursive YAML alias")
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlNullTag}
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		return effectiveNode(n.Alias, key, active, findings)
	}
	out := *n
	out.Content = nil
	switch n.Kind {
	case yaml.MappingNode:
		// Select raw entries first, before expanding any value. In particular,
		// an invalid inherited type must not survive an explicit override.
		entries := mergeEntries(n, key, map[*yaml.Node]bool{}, findings)
		for i := 0; i+1 < len(entries); i += 2 {
			k := entries[i]
			out.Content = append(out.Content, k, effectiveNode(entries[i+1], configKey(key, k.Value), active, findings))
		}
	case yaml.SequenceNode:
		for i, child := range n.Content {
			out.Content = append(out.Content, effectiveNode(child, fmt.Sprintf("%s[%d]", key, i), active, findings))
		}
	}
	return &out
}

func mergeEntries(n *yaml.Node, key string, active map[*yaml.Node]bool, findings *[]string) []*yaml.Node {
	if active[n] {
		*findings = append(*findings, configKey("", key)+": recursive YAML merge")
		return nil
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		return mergeEntries(n.Alias, key, active, findings)
	}
	if n.Kind != yaml.MappingNode {
		*findings = append(*findings, configKey("", key)+": YAML merge requires a mapping")
		return nil
	}
	var entries, merges []*yaml.Node
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Tag == "!!merge" {
			if seen[k.Value] {
				*findings = append(*findings, configKey(key, k.Value)+": duplicate key")
			}
			seen[k.Value] = true
			if v.Kind == yaml.SequenceNode {
				merges = append(merges, v.Content...)
			} else {
				merges = append(merges, v)
			}
			continue
		}
		if seen[k.Value] {
			*findings = append(*findings, configKey(key, k.Value)+": duplicate key")
			continue
		}
		seen[k.Value] = true
		entries = append(entries, k, v)
	}
	for _, source := range merges {
		inherited := mergeEntries(source, key, active, findings)
		for i := 0; i+1 < len(inherited); i += 2 {
			if !seen[inherited[i].Value] {
				seen[inherited[i].Value] = true
				entries = append(entries, inherited[i], inherited[i+1])
			}
		}
	}
	return entries
}

// checkNode returns a decodable copy, omitting only unknown keys and leaves
// whose types failed. Independent valid fields remain available to value rules.
func checkNode(n *yaml.Node, typ reflect.Type, key string, findings, invalid *[]string) *yaml.Node {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if n.ShortTag() == yamlNullTag {
		return n
	}
	out := *n
	out.Content = nil
	switch {
	case typ.Kind() == reflect.Struct && typ != reflect.TypeFor[Duration]() && n.Kind == yaml.MappingNode:
		fields := yamlFields(typ)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, value := n.Content[i], n.Content[i+1]
			p := configKey(key, k.Value)
			field, ok := fields[k.Value]
			if !ok {
				*findings = append(*findings, p+": unknown key")
				continue
			}
			checked := checkNode(value, field, p, findings, invalid)
			out.Content = append(out.Content, k, checked)
		}
	case typ.Kind() == reflect.Map && n.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			out.Content = append(out.Content, k, checkNode(n.Content[i+1], typ.Elem(), configKey(key, k.Value), findings, invalid))
		}
	case typ.Kind() == reflect.Slice && n.Kind == yaml.SequenceNode:
		for i, value := range n.Content {
			out.Content = append(out.Content, checkNode(value, typ.Elem(), fmt.Sprintf("%s[%d]", key, i), findings, invalid))
		}
	default:
		if err := n.Decode(reflect.New(typ).Interface()); err != nil {
			*findings = append(*findings, fmt.Sprintf("%s: invalid value for %s (line %d)", configKey("", key), typ, n.Line))
			*invalid = append(*invalid, key)
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlNullTag}
		}
		if n.Kind == yaml.ScalarNode && n.Value != "" {
			switch {
			case localInput(key):
				checkInput(n.Value, key, findings)
			case strings.HasPrefix(key, "secret.ageIdentities[") && strings.HasSuffix(key, "].ref") && strings.HasPrefix(n.Value, "file://"):
				checkInput(strings.TrimPrefix(n.Value, "file://"), key, findings)
			}
		}
		return n
	}
	return &out
}

func yamlFields(typ reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	// Provider's custom unmarshaler supports both inline and nested usage.
	if typ == reflect.TypeFor[Provider]() {
		out["usage"] = reflect.TypeFor[Usage]()
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if opts == "inline" {
			for k, t := range yamlFields(f.Type) {
				out[k] = t
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f.Type
	}
	return out
}

func configKey(parent, key string) string {
	if parent == "" {
		if key == "" {
			return "config"
		}
		return key
	}
	return parent + "." + key
}

func localInput(key string) bool {
	// These are local input files, not generated files or remote paths.
	switch key {
	case "supervisor.reviewBrief", "supervisor.workerBrief", "guide.instructions", "reporter.brief",
		"omp.modelsFile", "codex.authFile", "codex.configFile", "run.remote.kubeconfig", "agents.execution.rulesFile",
		"secret.tokenFile", "serve.gateway.tokenFile", "serve.identity.tokenFile":
		return true
	}
	return strings.HasPrefix(key, "kube.kubeconfig[") || strings.HasPrefix(key, "alerts.kubeconfig[")
}

func checkInput(path, key string, findings *[]string) {
	home, err := userHomeDir()
	if err != nil {
		*findings = append(*findings, key+": cannot resolve home directory")
		return
	}
	info, err := os.Stat(homePath(home, path))
	switch {
	case os.IsNotExist(err):
		*findings = append(*findings, key+": referenced file does not exist")
	case err != nil:
		*findings = append(*findings, key+": cannot stat referenced file")
	case !info.Mode().IsRegular():
		*findings = append(*findings, key+": referenced path is not a regular file")
	}
}
