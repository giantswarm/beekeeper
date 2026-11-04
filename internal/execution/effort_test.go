package execution

import (
	"encoding/json"
	"testing"
)

const testEffortHigh = "high"

func TestEffortRequestValidationAndPersistence(t *testing.T) {
	f := testFixture(t)
	base := f.r.Digest()
	for _, level := range []string{"", "low", "medium", testEffortHigh, "xhigh", "max", "invalid"} {
		r := f.r
		r.Effort = level
		if err := r.Validate(); (err == nil) != (level != "invalid") {
			t.Fatalf("effort %q validation: %v", level, err)
		}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var restored StartRequest
		if err := json.Unmarshal(data, &restored); err != nil || restored.Effort != level {
			t.Fatalf("effort %q round trip: %+v %v", level, restored, err)
		}
		if (restored.Digest() == base) != (level == "") {
			t.Fatalf("effort %q was not bound to the execution digest", level)
		}
	}
}
