package harness

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestNamespaceSpreadsConcurrentScenarios starts many scenarios at once, as TestScenarios does with
// t.Parallel: picking the least loaded API server must not race with another scenario's pick
// (go test -race), and the pool stays balanced.
func TestNamespaceSpreadsConcurrentScenarios(t *testing.T) {
	const servers, scenarios = 3, 60
	e := &Env{}
	for range servers {
		e.apis = append(e.apis, &apiServer{cfg: &rest.Config{}, kube: fake.NewClientBuilder().WithScheme(Scheme()).Build()})
	}

	// t.Parallel releases every parallel subtest at once when the parent's body returns.
	start := make(chan struct{})
	var wg sync.WaitGroup
	releases := make(chan func(), scenarios)
	for i := range scenarios {
		wg.Go(func() {
			<-start
			_, _, _, release, err := e.Namespace(context.Background(), fmt.Sprintf("scenario-%d", i))
			if err != nil {
				t.Error(err)
				return
			}
			releases <- release
		})
	}
	close(start)
	wg.Wait()
	close(releases)

	for i, a := range e.apis {
		if a.load != scenarios/servers {
			t.Errorf("API server %d serves %d scenarios, want %d", i, a.load, scenarios/servers)
		}
	}
	for release := range releases {
		release()
	}
	for i, a := range e.apis {
		if a.load != 0 {
			t.Errorf("API server %d serves %d scenarios after every release, want 0", i, a.load)
		}
	}
}
