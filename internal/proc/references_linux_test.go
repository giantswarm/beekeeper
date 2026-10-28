package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type referenceOwnedInfo struct {
	os.FileInfo
	uid uint32
}

func (i referenceOwnedInfo) Sys() any {
	info := *i.FileInfo.Sys().(*syscall.Stat_t)
	info.Uid = i.uid
	return &info
}

func TestUserReferencesChecksPtraceGatedChildOwnership(t *testing.T) {
	for _, foreignChild := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign child=%v", foreignChild), func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "42")
			if err := os.MkdirAll(filepath.Join(dir, "fd"), 0700); err != nil {
				t.Fatal(err)
			}
			uid := os.Getuid()
			foreignUID := uint32(0)
			if uid == 0 {
				foreignUID = 1
			}
			for name, data := range map[string]string{fixtureStatus: fmt.Sprintf("Uid: %d %d %d %d\n", uid, uid, uid, uid), "comm": "helper", fixtureEnvironment: "", fixtureMappings: ""} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			files := referenceFiles()
			files.stat = func(path string) (os.FileInfo, error) {
				info, err := os.Stat(path)
				if err == nil && foreignChild && filepath.Base(path) == fixtureEnvironment {
					// The directory inode still belongs to the user, while this child
					// inode reflects a later change in dumpability.
					return referenceOwnedInfo{FileInfo: info, uid: foreignUID}, nil
				}
				return info, err
			}
			files.readlink = func(string) (string, error) { return "", os.ErrPermission }
			_, commands, err := userReferencesAt(root, uid, files)
			if foreignChild {
				if err != nil || len(commands) != 1 {
					t.Fatalf("protected child blocked scan: %v", err)
				}
			} else if err == nil {
				t.Fatal("dumpable process access failure was ignored")
			}
		})
	}
}
