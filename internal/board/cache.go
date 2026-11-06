package board

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Metadata is small and stable. Expire it after an hour so field changes
// are picked up; cache failures must never prevent board operations.
func (c *Client) metaPath() string {
	key := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", c.Board.Owner, c.Board.Project)))
	return filepath.Join(c.CacheDir, fmt.Sprintf("board-meta-%x.json", key[:8]))
}

func (c *Client) cachedMeta() *meta {
	if c.CacheDir == "" {
		return nil
	}
	path := c.metaPath()
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > time.Hour {
		return nil
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil
	}
	var m meta
	if json.Unmarshal(data, &m) != nil || m.ID == "" || m.Fields[StatusField].ID == "" {
		return nil
	}
	return &m
}

func (c *Client) cacheMeta(m *meta) {
	if c.CacheDir == "" {
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(c.CacheDir, ".board-meta-*")
	if err != nil {
		return
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(data)
	closeErr := f.Close()
	if err == nil && closeErr == nil {
		_ = os.Rename(f.Name(), c.metaPath())
	}
}
