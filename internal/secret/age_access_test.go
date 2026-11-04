package secret

import (
	"context"
	"errors"
	"testing"
)

func TestAutomaticAgeLookupRequiresVaultAccess(t *testing.T) {
	id := isolateAge(t)
	file := sopsFile(t, "", id.Recipient().String())
	ctx := context.Background()
	const missingToken = "missing-token"
	for _, mode := range []string{missingToken, "token", "session"} {
		t.Run(mode, func(t *testing.T) {
			f := &itemTools{items: map[string]string{AgeItemTitle(id.Recipient().String()): id.String()}}
			o := &Ops{Vault: ageVault, Run: f.run, Session: mode == "session"}
			if mode == "token" {
				o.Token = "synthetic-token"
			}
			_, err := o.values(ctx, Ref{File: file})
			if mode == missingToken {
				if !errors.Is(err, ErrVault) || len(f.calls) != 0 {
					t.Fatalf("automatic lookup: err=%v, calls=%v", err, f.calls)
				}
			} else if err != nil || len(f.calls) == 0 {
				t.Fatalf("allowed lookup: err=%v, calls=%v", err, f.calls)
			}
			f.calls = nil
			_, err = o.Recipients(ctx, file)
			if mode == missingToken {
				if !errors.Is(err, ErrVault) || len(f.calls) != 0 {
					t.Fatalf("recipient listing: err=%v, calls=%v", err, f.calls)
				}
			} else if err != nil || len(f.calls) != 1 {
				t.Fatalf("allowed listing: err=%v, calls=%v", err, f.calls)
			}
		})
	}
}
