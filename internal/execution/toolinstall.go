package execution

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	maxToolArchive   = 256 << 20
	toolBinDirectory = "bin"
	goArchivePrefix  = "go/"
)

var toolHTTPClient = &http.Client{Timeout: 5 * time.Minute}

// ProvisionTools installs missing pinned tools, raising the Go pin when the
// served repositories require a newer release and never overwriting an existing
// installation. Call only while admission is quiesced. Installed tools are
// shared host state, not worker scratch.
func (s *Server) ProvisionTools(ctx context.Context) error {
	return s.ProvisionToolsFor(ctx, nil)
}

// ProvisionToolsFor installs missing pinned tools and raises the Go pin to the
// highest version named by the host's served repositories or required.
func (s *Server) ProvisionToolsFor(ctx context.Context, required map[string]string) error {
	if err := s.LoadPinnedTools(); err != nil {
		return err
	}
	need, err := s.requiredTools()
	if err != nil {
		return err
	}
	wantGo := need[ToolGo]
	if v := required[ToolGo]; v != "" && (wantGo == "" || semver.Compare(goSemver(v), goSemver(wantGo)) > 0) {
		wantGo = v
	}
	tools := maps.Clone(s.Config.Tools)
	baseGo, hasGo := tools[ToolGo]
	raisedGo := false
	if hasGo && wantGo != "" && semver.Compare(goSemver(wantGo), goSemver(baseGo.Version)) > 0 {
		next, err := RaiseGoTool(baseGo, wantGo)
		if err != nil {
			return err
		}
		tools[ToolGo] = next
		raisedGo = true
	}
	for _, name := range []string{ToolGo, ToolGitHub} {
		tool, ok := tools[name]
		if !ok {
			continue
		}
		if err := tool.validate(name); err != nil {
			return err
		}
		if err := s.probeTool(ctx, name, tool); err == nil {
			continue
		}
		if _, err := os.Lstat(tool.Executable); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s installation exists but does not match its pin; configure a new installation path", name)
		}
		if err := installTool(ctx, name, tool); err != nil {
			return fmt.Errorf("provisioning %s: %w", name, err)
		}
		if err := s.probeTool(ctx, name, tool); err != nil {
			return err
		}
	}
	if raisedGo {
		if s.Root != "" {
			if err := ownedDirectory(s.Root); err != nil {
				return err
			}
			if err := atomicFile(filepath.Join(s.Root, PinnedToolsFile), mustJSON(map[string]ToolConfig{ToolGo: tools[ToolGo]})); err != nil {
				return err
			}
		}
		s.Config.Tools = tools
	}
	return nil
}

func toolDownload(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tool download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("tool download exceeds size limit")
	}
	return data, nil
}

func installTool(ctx context.Context, name string, tool ToolConfig) error {
	return installToolWithClient(ctx, name, tool, toolHTTPClient)
}

func installToolWithClient(ctx context.Context, name string, tool ToolConfig, client *http.Client) error {
	if runtime.GOOS != "linux" {
		return errors.New("automatic execution tool provisioning requires Linux")
	}
	archiveName, url, checksum, prefix := "", "", "", ""
	if name == ToolGo {
		archiveName = "go" + tool.Version + "." + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
		metadata, err := toolDownload(ctx, client, "https://go.dev/dl/?mode=json&include=all", 16<<20)
		if err != nil {
			return err
		}
		var releases []struct {
			Files []struct {
				Filename string
				SHA256   string
			}
		}
		if err := json.Unmarshal(metadata, &releases); err != nil {
			return err
		}
		for _, release := range releases {
			for _, file := range release.Files {
				if file.Filename == archiveName {
					checksum = file.SHA256
				}
			}
		}
		url, prefix = "https://go.dev/dl/"+archiveName, goArchivePrefix
	} else {
		base := "gh_" + tool.Version
		archiveName = base + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
		root := "https://github.com/cli/cli/releases/download/v" + tool.Version + "/"
		metadata, err := toolDownload(ctx, client, root+base+"_checksums.txt", 1<<20)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(metadata), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == archiveName {
				checksum = fields[0]
			}
		}
		url, prefix = root+archiveName, strings.TrimSuffix(archiveName, ".tar.gz")+"/"
	}
	sum, err := hex.DecodeString(checksum)
	if err != nil || len(sum) != sha256.Size {
		return errors.New("official tool checksum not found")
	}
	data, err := toolDownload(ctx, client, url, maxToolArchive)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != strings.ToLower(checksum) {
		return errors.New("tool archive checksum mismatch")
	}
	// Go needs its complete GOROOT, whereas gh is a standalone executable.
	if name == ToolGo && filepath.Base(filepath.Dir(tool.Executable)) != toolBinDirectory {
		return errors.New("go executable must be under a dedicated installation's bin directory")
	}
	target := filepath.Dir(filepath.Dir(tool.Executable))
	if name == ToolGitHub {
		target = filepath.Dir(tool.Executable)
	}
	parent := filepath.Dir(target)
	if name == ToolGitHub {
		parent = target
	}
	if err := ownedDirectory(parent); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".beekeeper-tool-")
	if err != nil {
		return err
	}
	// A completed or failed download leaves no worker-owned installation files.
	defer func() { _ = os.RemoveAll(stage) }()
	if err := unpackTool(data, prefix, stage, name == ToolGitHub); err != nil {
		return err
	}
	if name == ToolGitHub {
		// Link provides no-replace semantics, even if another installer raced us.
		return os.Link(filepath.Join(stage, toolBinDirectory, ToolGitHub), tool.Executable)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return errors.New("go installation directory exists; use a new dedicated path")
	}
	return os.Rename(stage, target)
}

// unpackTool accepts only regular files and directories within the official
// prefix. Links and traversal are refused, even for a verified download.
func unpackTool(data []byte, prefix, dest string, ghOnly bool) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var total int64
	found := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !strings.HasPrefix(h.Name, prefix) {
			return errors.New("tool archive has unexpected prefix")
		}
		relative := strings.TrimPrefix(h.Name, prefix)
		if relative == "" {
			if h.Typeflag != tar.TypeDir || h.Size != 0 {
				return errors.New("tool archive root must be an empty directory entry")
			}
			continue
		}
		relative = strings.TrimSuffix(relative, "/")
		if path.Clean(relative) != relative || strings.HasPrefix(relative, "../") || path.IsAbs(relative) || relative == ".." || strings.Contains(relative, "\\") {
			return errors.New("tool archive path escapes installation")
		}
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			return errors.New("tool archive contains unsupported entry")
		}
		if h.Size < 0 || h.Size > (1<<30)-total {
			return errors.New("expanded tool archive exceeds size limit")
		}
		total += h.Size
		if ghOnly && relative != "bin/gh" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(relative))
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if h.Mode&0111 != 0 {
			mode = 0700
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // archive path validated above, staging directory is owned and link-free
		if err != nil {
			return err
		}
		_, err = io.CopyN(f, tr, h.Size)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if relative == "bin/go" || relative == "bin/gh" {
			found = true
		}
	}
	if !found {
		return errors.New("tool executable missing from archive")
	}
	return nil
}
