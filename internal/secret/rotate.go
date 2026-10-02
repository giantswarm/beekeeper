package secret

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// Carrier is a SOPS path that carried a rotated value: the value itself,
// or its base64 form (a Kubernetes Secret's data).
type Carrier struct {
	Ref    string `json:"ref"`
	Base64 bool   `json:"base64,omitempty"`
}

// Rotation is what rotate answers: the rotated reference, the new value's
// fingerprint and the SOPS paths that carry it now.
type Rotation struct {
	Ref         string    `json:"ref"`
	Fingerprint string    `json:"fingerprint"`
	Carriers    []Carrier `json:"carriers"`
}

// RotateGenerated replaces a value beekeeper generated: a new one into the
// shared vault's field first, then into every path of files that carried
// the old one, matched by fingerprint. Index matches and learns the values.
func (o *Ops) RotateGenerated(ctx context.Context, ref Ref, files []string, ix *guard.Index, length int, charset string) (Rotation, error) {
	if err := o.checkRotate(ref); err != nil {
		return Rotation{}, err
	}
	old, err := o.value(ctx, ref)
	if err != nil {
		return Rotation{}, err
	}
	v, err := Generate(length, charset)
	if err != nil {
		return Rotation{}, err
	}
	return o.carry(ctx, ref, files, ix, ix.Fingerprint(strings.TrimSpace(old)), v, true)
}

// RotateIssued carries a value a third party issued, which the person put
// into the shared vault's field, into every path of files that carried the
// field's old value: the one the index fingerprinted.
func (o *Ops) RotateIssued(ctx context.Context, ref Ref, files []string, ix *guard.Index) (Rotation, error) {
	if err := o.checkRotate(ref); err != nil {
		return Rotation{}, err
	}
	oldFP, ok := ix.ValueFingerprint(ref.String())
	if !ok {
		return Rotation{}, fmt.Errorf("%s: the index holds no fingerprint of its old value, so its carriers are unknown: "+
			"beekeeper scan index records it from scan.vaults before the value changes", ref)
	}
	v, err := o.value(ctx, ref)
	if err != nil {
		return Rotation{}, err
	}
	if ix.Fingerprint(strings.TrimSpace(v)) == oldFP {
		return Rotation{}, fmt.Errorf("%s still holds the value the index recorded: rotate it at its issuer into the vault first, "+
			"or --generate a new one for a value beekeeper made", ref)
	}
	return o.carry(ctx, ref, files, ix, oldFP, v, false)
}

// checkRotate refuses a reference that is not a field of the shared vault,
// and an Ops without a fingerprint.
func (o *Ops) checkRotate(ref Ref) error {
	if ref.Op == "" {
		return fmt.Errorf("%s: rotate takes the shared vault's field, op://<vault>/<item>/<field>, the value's source", ref)
	}
	return o.checkVault(ref)
}

// carry writes v to ref first when store is set, then to every carrier of
// the value fingerprinted oldFP, and indexes v under each reference. The
// carriers are found, every file decrypted, before anything is written:
// a file that cannot be read stops the rotation with nothing changed.
func (o *Ops) carry(ctx context.Context, ref Ref, files []string, ix *guard.Index, oldFP, v string, store bool) (Rotation, error) {
	docs := map[string]*document{}
	var carriers []Carrier
	for _, f := range files {
		doc, err := o.decrypt(ctx, f)
		if err != nil {
			return Rotation{}, fmt.Errorf("nothing rotated: %w", err)
		}
		leaves := doc.leaves()
		for _, path := range sortedKeys(leaves) {
			b64, ok := carries(ix, leaves[path], oldFP)
			if !ok {
				continue
			}
			nv := v
			if b64 {
				nv = base64.StdEncoding.EncodeToString([]byte(v))
			}
			if err := doc.set(path, nv); err != nil {
				return Rotation{}, fmt.Errorf("nothing rotated: %s: %w", f, err)
			}
			docs[f] = doc
			carriers = append(carriers, Carrier{Ref: Ref{File: f, Path: path}.String(), Base64: b64})
		}
	}
	if store {
		if err := o.storeVault(ctx, ref, v); err != nil {
			return Rotation{}, fmt.Errorf("nothing rotated: %w", err)
		}
	}
	rot := Rotation{Ref: ref.String(), Fingerprint: o.Fingerprint(v), Carriers: []Carrier{}}
	var errs []error
	for _, f := range slices.Sorted(maps.Keys(docs)) {
		if err := o.encrypt(ctx, docs[f], f); err != nil {
			errs = append(errs, err)
			continue
		}
		for _, c := range carriers {
			if file, path, _ := strings.Cut(c.Ref, "#"); file == f {
				rot.Carriers = append(rot.Carriers, c)
				ix.Add(guard.SOPSRef+file+"#"+path, v)
			}
		}
	}
	ix.Add(ref.String(), v)
	if len(errs) > 0 {
		return rot, fmt.Errorf("the vault and %d of %d carriers hold the new value, these not: %w",
			len(rot.Carriers), len(carriers), errors.Join(errs...))
	}
	return rot, nil
}

// carries is whether leaf is the value fingerprinted oldFP, or its base64
// form (b64).
func carries(ix *guard.Index, leaf, oldFP string) (b64, ok bool) {
	leaf = strings.TrimSpace(leaf)
	if ix.Fingerprint(leaf) == oldFP {
		return false, true
	}
	if d, err := base64.StdEncoding.DecodeString(leaf); err == nil && ix.Fingerprint(strings.TrimSpace(string(d))) == oldFP {
		return true, true
	}
	return false, false
}

// PlatformRef is a credential the platform manager generates:
// platform://<installation>/<capability>/<name>, the name as the manager's
// dry run lists its generated secrets.
type PlatformRef struct {
	Installation, Capability, Name string
}

// PlatformPrefix opens a [PlatformRef].
const PlatformPrefix = "platform://"

// ParsePlatformRef reads a platform:// reference.
func ParsePlatformRef(s string) (PlatformRef, error) {
	parts := strings.Split(strings.TrimPrefix(s, PlatformPrefix), "/")
	if len(parts) != 3 || slicesHasEmpty(parts) {
		return PlatformRef{}, fmt.Errorf("%q: a platform reference is platform://<installation>/<capability>/<name>", s)
	}
	return PlatformRef{Installation: parts[0], Capability: parts[1], Name: parts[2]}, nil
}

func (r PlatformRef) String() string {
	return PlatformPrefix + r.Installation + "/" + r.Capability + "/" + r.Name
}

// RotatePlatform rotates a platform manager credential with the manager's
// own rotation, run on the host: platformctl reconciles the capability
// with --rotate, the manager writes the new value into the installation's
// SOPS files in a pull request, and nothing is decrypted here. dryRun
// shows the files that hold it instead. It answers platformctl's output,
// redacted.
func (o *Ops) RotatePlatform(ctx context.Context, r PlatformRef, reason string, dryRun bool) (string, error) {
	if strings.TrimSpace(reason) == "" {
		return "", errors.New("--reason: the platform manager records why a credential is rotated")
	}
	mode := "--commit"
	if dryRun {
		mode = "--dry-run"
	}
	out, err := o.Run(ctx, "", nil, nil, "platformctl", "installation", "reconcile", r.Installation, r.Capability,
		mode, "--reason", reason, "--rotate", r.Name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", r, err)
	}
	return redact(string(out), "", r.String()), nil
}
