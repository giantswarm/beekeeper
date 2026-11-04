package lease

import "testing"

func TestDestructiveProof(t *testing.T) {
	for _, p := range []string{"node-restart proof", "Node restart proof", "down/up proof"} {
		if !IsDestructiveProof(p) {
			t.Errorf("missed %q", p)
		}
	}
	for _, p := range []string{"inspect nodes", "download proof", "shutdown investigation"} {
		if IsDestructiveProof(p) {
			t.Errorf("misclassified %q", p)
		}
	}
	d := Dir(t.TempDir())
	for _, purpose := range []string{"down/up proof", "inspect"} {
		if _, err := d.Claim(lab, Holder{Env: lab, Purpose: purpose}); err != nil {
			t.Fatal(err)
		}
		if err := d.Release(lab); err != nil {
			t.Fatal(err)
		}
		p, err := d.PreviousProof(lab)
		if err != nil {
			t.Fatal(err)
		}
		if (p != nil) != IsDestructiveProof(purpose) {
			t.Fatalf("previous proof: %+v", p)
		}
	}
}
