package harness

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
)

// MergeConfig merges a config_apply's teams, people and installation onto c (by name, as the files
// Flux applies would be edited) and returns the result and which objects changed.
func MergeConfig(c Config, a map[string]any) (Config, error) {
	out := Config{Installation: c.Installation}
	if inst, ok := a["installation"]; ok {
		m, isMap := inst.(map[string]any)
		if !isMap {
			return c, fmt.Errorf("installation must be a map")
		}
		out.Installation = Merge(c.Installation, m).(map[string]any)
	}
	var err error
	if out.Teams, err = mergeList(c.Teams, a[argTeams], argTeams); err != nil {
		return c, err
	}
	if out.People, err = mergeList(c.People, a[argPeople], argPeople); err != nil {
		return c, err
	}
	if _, err := out.InstallationSpec(); err != nil {
		return c, fmt.Errorf("installation: %w", err)
	}
	if _, err := out.TeamConfigs(); err != nil {
		return c, err
	}
	if _, err := out.PersonSpecs(); err != nil {
		return c, err
	}
	return out, nil
}

func mergeList(base []map[string]any, over any, what string) ([]map[string]any, error) {
	if over == nil {
		return base, nil
	}
	l, ok := over.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", what)
	}
	b := make([]any, len(base))
	for i, m := range base {
		b[i] = m
	}
	merged := b
	if len(l) > 0 {
		merged, ok = Merge(b, l).([]any)
		if !ok {
			return nil, fmt.Errorf("%s: can't merge", what)
		}
	}
	out := make([]map[string]any, len(merged))
	for i, e := range merged {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] is not a map", what, i)
		}
		out[i] = m
	}
	return out, nil
}

// ApplyConfig creates or updates the configuration objects: the Installation (named by its org), each
// Team (by name) and each Person (by login), the way Flux applies them.
func ApplyConfig(ctx context.Context, c client.Client, ns string, cfg Config) error {
	spec, err := cfg.InstallationSpec()
	if err != nil {
		return err
	}
	if err := apply(ctx, c, &v1alpha1.Installation{ObjectMeta: meta(ns, spec.Org)}, func(o client.Object) {
		o.(*v1alpha1.Installation).Spec = spec
	}); err != nil {
		return fmt.Errorf("installation: %w", err)
	}
	teams, err := cfg.TeamConfigs()
	if err != nil {
		return err
	}
	for _, t := range teams {
		t := t
		if err := apply(ctx, c, &v1alpha1.Team{ObjectMeta: meta(ns, t.Name)}, func(o client.Object) {
			o.(*v1alpha1.Team).Spec = t.TeamSpec
		}); err != nil {
			return fmt.Errorf("team %s: %w", t.Name, err)
		}
	}
	people, err := cfg.PersonSpecs()
	if err != nil {
		return err
	}
	for _, p := range people {
		p := p
		if err := apply(ctx, c, &v1alpha1.Person{ObjectMeta: meta(ns, v1alpha1.PersonName(p.Login))}, func(o client.Object) {
			o.(*v1alpha1.Person).Spec = p
		}); err != nil {
			return fmt.Errorf("person %s: %w", p.Login, err)
		}
	}
	return nil
}

func meta(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }

// apply creates obj with set applied, or updates the existing object's spec.
func apply(ctx context.Context, c client.Client, obj client.Object, set func(client.Object)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		key := client.ObjectKeyFromObject(obj)
		err := c.Get(ctx, key, obj)
		switch {
		case apierrors.IsNotFound(err):
			obj.SetResourceVersion("")
			set(obj)
			return c.Create(ctx, obj)
		case err != nil:
			return err
		}
		set(obj)
		return c.Update(ctx, obj)
	})
}

func configApply(ctx context.Context, w *World, a map[string]any) (any, error) {
	cfg, err := MergeConfig(w.Config, a)
	if err != nil {
		return nil, err
	}
	if w.Kube == nil {
		return nil, fmt.Errorf("no API server")
	}
	if err := ApplyConfig(ctx, w.Kube, w.Namespace, cfg); err != nil {
		return nil, err
	}
	w.Config = cfg
	return map[string]any{"applied": true}, nil
}
