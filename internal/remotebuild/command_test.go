package remotebuild

import "testing"

// goTest is the go subcommand the offload cases run.
const goTest = "test"

func TestToolchainShellInterpretation(t *testing.T) {
	for _, script := range []string{
		`go test -e\xec=host-helper ./...`,
		`go test -tool\exec=host-helper ./...`,
		`go test \/host/project`,
		`go test $'\x2fhost/project'`,
		`go test ./*`,
		`go test ./package?`,
		`go test ./package[ab]`,
		`go test ~/project`,
		`go test ./{one,two}`,
		`go test ./package{1..3}`,
		`go test -run "Test\\Name" ./...`,
		`go test $"translated" ./...`,
	} {
		t.Run(script, func(t *testing.T) {
			if got := Toolchain([]string{shellBash, "-c", script}); got != "" {
				t.Fatalf("Toolchain = %q, want local execution", got)
			}
		})
	}
	for _, argv := range [][]string{
		{shellBash, "-c", `go test -run 'Test[AB].*' ./...`},
		{shellBash, "-c", `go test -run "Test[AB].*" ./...`},
		{shellBash, "-c", `go test -run Test'[AB].*' ./...`},
		{shellBash, "-c", `go test -run 'Test{A,B}~?' ./...`},
		{"go", goTest, "-run", "Test[AB].*", testPackages},
	} {
		if got := Toolchain(argv); got != "go" {
			t.Errorf("Toolchain(%q) = %q, want go", argv, got)
		}
	}
}
