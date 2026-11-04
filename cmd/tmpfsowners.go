package cmd

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// tmpfsLargest is read only and runs only above the tmpfs warning threshold.
// References associate files with commands and their owning worker sessions;
// files left behind by exited or unreadable processes have unknown owners.
func tmpfsLargest(root string, table *proc.Table, owners map[int]string, refs func(int) ([]string, error)) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "largest entries unavailable"
	}
	type entry struct {
		name    string
		bytes   int64
		partial bool
	}
	largest := make([]entry, 0, len(entries))
	for _, e := range entries {
		u := diskprune.Inspect(filepath.Join(root, e.Name()))
		largest = append(largest, entry{e.Name(), u.Bytes, u.Unreadable})
	}
	slices.SortFunc(largest, func(a, b entry) int { return cmp.Or(cmp.Compare(b.bytes, a.bytes), cmp.Compare(a.name, b.name)) })
	if len(largest) > 3 {
		largest = largest[:3]
	}
	attributed := map[string][]string{}
	if table != nil {
		for pid, p := range table.ByPID {
			paths, _ := refs(pid)
			label := fmt.Sprintf("%s pid %d", p.Comm, pid)
			if s := owner(table, pid, owners); s != "" {
				label += fmt.Sprintf(" (%q)", s)
			}
			for _, e := range largest {
				base := filepath.Join(root, e.name)
				for _, path := range paths {
					if path == base || strings.HasPrefix(path, base+string(filepath.Separator)) {
						attributed[e.name] = append(attributed[e.name], label)
						break
					}
				}
			}
		}
	}
	parts := make([]string, 0, len(largest))
	for _, e := range largest {
		labels := attributed[e.name]
		slices.Sort(labels)
		labels = slices.Compact(labels)
		who := "owner unknown"
		if len(labels) > 0 {
			shown := labels
			if len(shown) > 3 {
				shown = shown[:3]
			}
			who = strings.Join(shown, ", ")
			if len(labels) > len(shown) {
				who += fmt.Sprintf(", +%d processes", len(labels)-len(shown))
			}
		}
		size := sizeStr(e.bytes)
		if e.partial {
			size += "+ (partly unreadable)"
		}
		parts = append(parts, fmt.Sprintf("%q %s [%s]", e.name, size, who))
	}
	if len(parts) == 0 {
		return "no visible entries (deleted open files may still use space)"
	}
	return "largest: " + strings.Join(parts, "; ")
}
