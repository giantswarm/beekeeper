package guard

import "testing"

func TestExecutionCapPreservesVerifiedStopOwner(t *testing.T) {
	o := Options{Max: "1G", Swap: "0", StopWith: "beekeeper-execution-fixture.service"} //nolint:gosec // fixed fake service identifier, not an authentication value
	for _, slot := range []int{0, 1} {
		if cap := o.cap(slot); cap.StopWith != o.StopWith {
			t.Fatal("capped run dropped its verified execution owner")
		}
	}
}
