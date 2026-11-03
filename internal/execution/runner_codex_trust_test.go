package execution

import (
	"reflect"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestCodexArgsWorkspaceTrust(t *testing.T) {
	for _, workspace := range []string{"/srv/workspaces/worker/workspace", "/srv/work spaces/quoted\"repo\\path"} {
		for _, thread := range []string{"", fixtureThread} {
			t.Run(workspace+thread, func(t *testing.T) {
				args := codexArgs("gpt-fixture", workspace, thread)
				var override string
				for i := 0; i+1 < len(args); i++ {
					if args[i] == "-c" {
						override = args[i+1]
					}
				}
				var got map[string]any
				if err := toml.Unmarshal([]byte(override), &got); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{"projects": map[string]any{workspace: map[string]any{"trust_level": "trusted"}}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("trust override = %#v, want %#v", got, want)
				}
			})
		}
	}
}
