package machine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSwapUsedLimit(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		conf  string
		want  int
		found bool
	}{
		{"[OOM]\nSwapUsedLimit=80%\n", 80, true},
		{"[OOM]\n#SwapUsedLimit=80%\nDefaultMemoryPressureLimit=60%\n", 0, false},
		{"[OOM]\nSwapUsedLimit = 955‰\n", 95, true},
		{"[OOM]\nSwapUsedLimit=\n", 0, false},
	} {
		p := filepath.Join(dir, "oomd.conf")
		if err := os.WriteFile(p, []byte(c.conf), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, found := readSwapUsedLimit(p); got != c.want || found != c.found {
			t.Errorf("%q: %d %v, want %d %v", c.conf, got, found, c.want, c.found)
		}
	}
}

func TestOOMDHeadroom(t *testing.T) {
	if got := (Mem{SwapTotalMiB: 16383, SwapUsedMiB: 10627}).OOMDHeadroomMiB(90); got != 4117 {
		t.Errorf("headroom %d", got)
	}
}
