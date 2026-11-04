package alerts

import (
	"context"
	"os"
	"path/filepath"
	"slices"
)

// EnsureContexts fills Login for every configured target Teleport lists.
// It inspects only Login: a context in another kubeconfig must not hide a
// missing entry in the keeper's file. Failures are returned per installation.
func (d Discoverer) EnsureContexts(ctx context.Context, targets []Target) []string {
	if len(targets) == 0 {
		return nil
	}
	failure := func(reason string) []string {
		var out []string
		for _, t := range targets {
			out = append(out, t.Name+": "+reason)
		}
		return out
	}
	if d.Login == "" {
		return failure("no keeper kubeconfig configured")
	}
	// Discovery exclusions do not apply to explicitly configured installations.
	d.Exclude = nil
	names, err := d.clusters(ctx)
	if err != nil {
		return failure("could not list Teleport clusters")
	}
	if err := os.MkdirAll(filepath.Dir(d.Login), 0o700); err != nil {
		return failure("could not create keeper kubeconfig directory")
	}
	contexts := append([]string{}, kubeContexts(ctx, d.Kubectl, []string{d.Login})...)
	var unread []string
	for _, t := range targets {
		switch {
		case !slices.Contains(names, t.Name):
			unread = append(unread, t.Name+": not listed by Teleport")
		case ResolveContext(t.Name, t.Context, d.Template, contexts) != "":
			continue
		default:
			if err := d.login(ctx, t.Name); err != nil {
				unread = append(unread, t.Name+": tsh kube login failed")
				continue
			}
			contexts = append([]string{}, kubeContexts(ctx, d.Kubectl, []string{d.Login})...)
			if ResolveContext(t.Name, t.Context, d.Template, contexts) == "" {
				unread = append(unread, t.Name+": login wrote no matching kube context")
			}
		}
	}
	return unread
}
