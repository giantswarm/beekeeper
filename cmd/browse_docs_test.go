package cmd

import (
	"os"
	"strings"
	"testing"

)

func TestBrowseDocsRouteXReads(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"README":       string(readme),
		"help":         (&app{}).browseCmd().Long,
	} {
		t.Run(name, func(t *testing.T) {
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, "x.com reads") && strings.Contains(line, "beekeeper browse") {
					if strings.Contains(text, "WebFetch returns HTTP 402") {
						return
					}
				}
			}
			t.Error("missing x.com read guidance through beekeeper browse and WebFetch HTTP 402 explanation")
		})
	}
}
