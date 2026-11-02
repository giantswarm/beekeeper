package execution

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

const HarnessOmp = "omp"

// Pin the protocol and extension API family proven by this adapter.
const OmpVersions = ">= 18.14.2, < 19.0.0"

//go:embed omp_guard.js
var ompGuard string

func (s *Server) ompGuardFile() string { return filepath.Join(s.Root, "omp-guard.mjs") }
func (s *Server) ompGuardBytes() []byte {
	return []byte("const BEEKEEPER_BINARY = " + string(mustJSON(s.Executable)) + ";\nconst BEEKEEPER_CONFIG = " + string(mustJSON(s.ConfigPath)) + ";\n" + ompGuard)
}
func (s *Server) ompEnvironment() []string {
	return append(cleanEnvironment("", s.Executable), "PI_CODING_AGENT_DIR="+s.Config.Harnesses[HarnessOmp].ConfigDir)
}
func (s *Server) ompArgs(model string) []string {
	args := []string{"--mode", "rpc", "--no-ui", "--no-extensions", "--extension", s.ompGuardFile(), "--tools", "bash,read,write", "--approval-mode", "yolo", "--append-system-prompt", filepath.Join(s.Root, "CLAUDE.md")}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}
func (s *Server) probeOmp(ctx context.Context, h HarnessConfig) error {
	version, err := s.command(ctx, s.Root, s.ompEnvironment(), h.Executable, "--version")
	if err != nil || strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(version)), "omp v")) != h.Version {
		return errors.New("unsupported or mismatched omp version")
	}
	raw, err := s.command(ctx, s.Root, s.ompEnvironment(), h.Executable, "models", "--json", "--no-extensions")
	var catalogue struct {
		Models []struct {
			Selector string `json:"selector"`
		} `json:"models"`
	}
	if err != nil || json.Unmarshal(raw, &catalogue) != nil || len(catalogue.Models) == 0 {
		return errors.New("omp has no authenticated model; provision the provider login independently on the execution host")
	}
	return s.probeOmpGuard(ctx, h)
}
