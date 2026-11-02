package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

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
		return []string{fmt.Sprintf("config: %v", err)}, nil
	}
	var findings []string
	if len(doc.Content) != 0 {
		checkNode(doc.Content[0], reflect.TypeFor[Config](), "", map[*yaml.Node]bool{}, &findings)
	}
	if len(findings) == 0 {
		c, err := parse(path, raw)
		if err != nil {
			findings = append(findings, err.Error())
		} else {
			findings = append(findings, c.Incomplete()...)
		}
	}
	return findings, nil
}

func checkNode(n *yaml.Node, typ reflect.Type, key string, active map[*yaml.Node]bool, findings *[]string) {
	if active[n] {
		*findings = append(*findings, key+": recursive YAML alias")
		return
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		checkNode(n.Alias, typ, key, active, findings)
		return
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if n.ShortTag() == yamlNullTag {
		return // YAML's zero value, as in Load.
	}
	switch {
	case typ.Kind() == reflect.Struct && typ != reflect.TypeFor[Duration]() && n.Kind == yaml.MappingNode:
		fields := yamlFields(typ)
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, value := n.Content[i], n.Content[i+1]
			if k.Tag == "!!merge" {
				checkMerge(value, typ, key, active, findings)
				continue
			}
			p := configKey(key, k.Value)
			field, ok := fields[k.Value]
			switch {
			case seen[k.Value]:
				*findings = append(*findings, p+": duplicate key")
			case !ok:
				*findings = append(*findings, p+": unknown key")
			default:
				checkNode(value, field, p, active, findings)
			}
			seen[k.Value] = true
		}
	case typ.Kind() == reflect.Map && n.Kind == yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, value := n.Content[i], n.Content[i+1]
			if k.Tag == "!!merge" {
				checkMerge(value, typ, key, active, findings)
				continue
			}
			p := configKey(key, k.Value)
			if seen[k.Value] {
				*findings = append(*findings, p+": duplicate key")
			} else {
				checkNode(value, typ.Elem(), p, active, findings)
			}
			seen[k.Value] = true
		}
	case typ.Kind() == reflect.Slice && n.Kind == yaml.SequenceNode:
		for i, value := range n.Content {
			checkNode(value, typ.Elem(), fmt.Sprintf("%s[%d]", key, i), active, findings)
		}
	default:
		// Decode leaves through the same custom unmarshallers as Load
		// (durations, token counts and scalar installation names included).
		// Do not echo the value: a hand edit may contain a credential.
		if err := n.Decode(reflect.New(typ).Interface()); err != nil {
			*findings = append(*findings, fmt.Sprintf("%s: invalid value for %s (line %d)", configKey("", key), typ, n.Line))
			return
		}
		if n.Kind == yaml.ScalarNode && n.Value != "" {
			switch {
			case localInput(key):
				checkInput(n.Value, key, findings)
			case strings.HasPrefix(key, "secret.ageIdentities[") && strings.HasSuffix(key, "].ref") && strings.HasPrefix(n.Value, "file://"):
				checkInput(strings.TrimPrefix(n.Value, "file://"), key, findings)
			}
		}
	}
}

func checkMerge(n *yaml.Node, typ reflect.Type, key string, active map[*yaml.Node]bool, findings *[]string) {
	if n.Kind == yaml.SequenceNode {
		for _, child := range n.Content {
			checkNode(child, typ, key, active, findings)
		}
		return
	}
	checkNode(n, typ, key, active, findings)
}

func yamlFields(typ reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
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
		"secret.tokenFile", "serve.gateway.tokenFile":
		return true
	}
	return strings.HasPrefix(key, "kube.kubeconfig[") || strings.HasPrefix(key, "alerts.kubeconfig[")
}

func checkInput(path, key string, findings *[]string) {
	home, err := os.UserHomeDir()
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
