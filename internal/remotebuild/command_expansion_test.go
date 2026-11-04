//nolint:goconst // Shell examples deliberately repeat the words the eligibility contract describes.
package remotebuild

import "testing"

func TestToolchainRejectsShellReinterpretation(t *testing.T) {
	for _, command := range []string{
		`go test -e\xec=host-helper ./...`,
		`go test -tool\exec=host-helper ./...`,
		`go test \/host/project`,
		`go test $'\x2fhost/project'`,
		`go test *.go`,
		`go test ~/project`,
		`go test ./pkg{one,two}`,
	} {
		t.Run(command, func(t *testing.T) {
			if got := Toolchain([]string{shellBash, "-c", command}); got != "" {
				t.Errorf("nonliteral shell command selected remote %q: %s", got, command)
			}
		})
	}
	// Quoted pattern characters are literal and keep ordinary test filters
	// eligible; quote handling must preserve the distinction from expansion.
	for _, command := range []string{`go test ./... -run 'Test[AB].*'`, `go test ./... -run "Test[AB].*"`} {
		if got := Toolchain([]string{shellBash, "-c", command}); got != "go" {
			t.Errorf("literal test filter became ineligible: %s", command)
		}
	}
}
