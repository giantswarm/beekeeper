// Command scenariolint strictly loads the scenario files, checks them against the registered step tools
// and the vocabulary of docs/phase1/scenarios.md, and regenerates CATALOGUE.md.
//
//	go run ./internal/testing/scenariolint internal/testing/scenarios
//	go run ./internal/testing/scenariolint -check internal/testing/scenarios
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/giantswarm/beekeeper/internal/testing/harness"
)

func main() {
	check := flag.Bool("check", false, "only check that CATALOGUE.md is current; don't write it")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "usage: scenariolint [-check] <scenarios dir>\n") // as flag's own usage output does
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	os.Exit(run(flag.Arg(0), *check))
}

func run(root string, check bool) int {
	scenarios, err := harness.LoadDir(root)
	if err != nil {
		fmt.Println(err)
		return 1
	}
	counts := map[string]int{}
	for _, s := range scenarios {
		counts[s.Concept]++
	}
	var cs []string
	for c, n := range counts {
		cs = append(cs, fmt.Sprintf("%s=%d", c, n))
	}
	sort.Strings(cs)
	fmt.Printf("%d scenarios, 0 problems\nconcepts: %s\n", len(scenarios), strings.Join(cs, " "))

	path := filepath.Join(root, "CATALOGUE.md")
	want := []byte(harness.Catalogue(scenarios))
	have, _ := os.ReadFile(path) //nolint:gosec // G304: the catalogue in the scenario directory the developer named
	if bytes.Equal(have, want) {
		fmt.Println(path, "is current")
		return 0
	}
	if check {
		fmt.Println(path, "is stale: run go run ./internal/testing/scenariolint", root)
		return 1
	}
	if err := os.WriteFile(path, want, 0o600); err != nil {
		fmt.Println(err)
		return 1
	}
	fmt.Println("wrote", path)
	return 0
}
