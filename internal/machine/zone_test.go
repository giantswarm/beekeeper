package machine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestZone(t *testing.T) {
	link := filepath.Join(t.TempDir(), "localtime")
	defer func(old string) { localtime = old }(localtime)
	localtime = link
	if err := os.Symlink("../usr/share/zoneinfo/Europe/Berlin", link); err != nil {
		t.Fatal(err)
	}
	if z, err := Zone(); err != nil || z.String() != "Europe/Berlin" {
		t.Errorf("Zone = %v, %v", z, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nowhere", link); err != nil {
		t.Fatal(err)
	}
	if _, err := Zone(); err == nil {
		t.Error("a link to no zoneinfo file: no error")
	}
}
