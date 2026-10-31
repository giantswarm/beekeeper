package execution

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"time"
)

const MergeSucceeded = "merged"

var mergeHeadPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// MergeAuthorization is coordinator-owned. It is never a bearer credential:
// every use also authenticates the exact owned execution with the live service.
type MergeAuthorization struct {
	ReviewDigest string       `json:"reviewDigest"`
	DeskReviewID string       `json:"deskReviewID"`
	ReviewID     string       `json:"reviewID"`
	ID           string       `json:"id"`
	Ref          Ref          `json:"ref"`
	Repository   string       `json:"repository"`
	PR           int          `json:"pr"`
	Head         string       `json:"head"`
	Issuer       string       `json:"issuer"`
	IssuedAt     time.Time    `json:"issuedAt"`
	ExpiresAt    time.Time    `json:"expiresAt"`
	Revoked      bool         `json:"revoked"`
	Consumed     bool         `json:"consumed"`
	Outcome      string       `json:"outcome,omitempty"`
	Audit        []MergeAudit `json:"audit"`
}

type MergeAudit struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
}

type MergeUse struct {
	ID         string `json:"id"`
	Ref        Ref    `json:"ref"`
	Repository string `json:"repository"`
	PR         int    `json:"pr"`
	Head       string `json:"head"`
}

type MergeResult struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
}

func (g MergeAuthorization) Admits(use MergeUse, now time.Time) error {
	if g.ID != use.ID || g.Ref != use.Ref || g.Repository != use.Repository || g.PR != use.PR || g.Head != use.Head || !mergeHeadPattern.MatchString(g.Head) || g.Ref.Validate() != nil || !repoPattern.MatchString(g.Repository) || g.PR < 1 || g.Revoked || g.Consumed || !g.ExpiresAt.After(now) || g.IssuedAt.After(now) || g.ExpiresAt.Sub(g.IssuedAt) > 5*time.Minute || g.Issuer == "" {
		return errors.New("missing, expired, consumed, revoked or mismatched merge authorization")
	}
	return nil
}

// The caller serializes this durable ledger with the coordinating state lock.
// It records consumption before invoking GitHub, so a crash can never replay a
// merge. Reconciliation may read GitHub but never repeats a consumed attempt.
func WriteMergeAuthorization(root string, grant MergeAuthorization, action string, now time.Time) error {
	if !ValidIdentity(grant.ID) {
		return errors.New("invalid merge authorization identity")
	}
	if err := ownedDirectory(root); err != nil {
		return err
	}
	grant.Audit = append(grant.Audit, MergeAudit{At: now.UTC(), Action: action})
	if len(grant.Audit) > 128 {
		grant.Audit = grant.Audit[len(grant.Audit)-128:]
	}
	return atomicFile(filepath.Join(root, grant.ID+".json"), mustJSON(grant))
}

func ReadMergeAuthorization(root, id string) (MergeAuthorization, error) {
	var grant MergeAuthorization
	if !ValidIdentity(id) {
		return grant, errors.New("invalid merge authorization identity")
	}
	if err := ownedDirectory(root); err != nil {
		return grant, err
	}
	err := readJSON(filepath.Join(root, id+".json"), &grant)
	if err == nil && grant.ID != id {
		err = errors.New("merge authorization identity mismatch")
	}
	return grant, err
}

// UseMerge performs one synchronous attempt under the caller's coordinating
// state transaction. Stop, holds and revocation therefore cannot commit between
// the live check and command execution. No detached child is permitted.
func UseMerge(ctx context.Context, root string, use MergeUse, now time.Time, live func(context.Context) error, merge func(context.Context) error) (MergeResult, error) {
	result := MergeResult{ID: use.ID}
	grant, err := ReadMergeAuthorization(root, use.ID)
	if err != nil {
		return result, err
	}
	if err := grant.Admits(use, now); err != nil {
		_ = WriteMergeAuthorization(root, grant, "denied", now)
		return result, err
	}
	if err := live(ctx); err != nil {
		_ = WriteMergeAuthorization(root, grant, "denied-live-check", now)
		return result, err
	}
	// Expiry is also the command deadline. Cancellation terminates the
	// synchronous merge rather than leaving a detached authorization user.
	ctx, cancel := context.WithDeadline(ctx, grant.ExpiresAt)
	defer cancel()
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	grant.Consumed, grant.Outcome = true, "uncertain"
	if err := WriteMergeAuthorization(root, grant, "used", now); err != nil {
		return result, err
	}
	result.Outcome = "uncertain"
	if err := merge(ctx); err != nil {
		return result, err
	}
	grant.Outcome, result.Outcome = MergeSucceeded, MergeSucceeded
	return result, WriteMergeAuthorization(root, grant, MergeSucceeded, time.Now())
}
