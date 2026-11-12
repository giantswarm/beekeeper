package loop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/findings"
	"github.com/giantswarm/beekeeper/internal/github"
)

// Receipts precede board intake, so an intake retry cannot replay an older
// description over newer facts. Keep identities rather than report text.
func (l *Loop) placeFinding(ctx context.Context, report findings.Report) (string, error) {
	const receiptsFile = "supervisor-findings.json"
	receipts := map[string]string{}
	if _, err := l.Host.Store().ReadFile(receiptsFile, &receipts); err != nil {
		return "", err
	}
	raw, err := json.Marshal([]string{strings.ToLower(report.Repo), strings.ToLower(strings.Join(strings.Fields(report.Title), " ")), strings.TrimSpace(report.Body)})
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%x", sha256.Sum256(raw))
	if url := receipts[key]; url != "" {
		return url, nil
	}
	var url string
	err = l.Host.LoopGitHub(ctx, "file finding", func() error {
		var err error
		url, err = findings.Place(ctx, github.RunGH, report)
		return err
	})
	if err != nil {
		return "", err
	}
	receipts[key] = url
	if err := l.Host.Store().WriteFile(receiptsFile, receipts); err != nil {
		return "", err
	}
	return url, nil
}
