package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SecretKeysOptions are the Secret a set of produced values goes into.
type SecretKeysOptions struct {
	// Keys are the Secret's keys the values may take, in the order
	// answered; a key the producer answers no value for stays as it is.
	Keys []string
	// New is the Secret an absent file starts as; nil refuses an absent
	// file.
	New *NewSecret
	// Replace overwrites keys the Secret holds already; without it such a
	// key refuses before the producer runs.
	Replace bool
	// Produce makes the values, each under one of Keys. It runs once,
	// after every check passed: a credential handed out once is not spent
	// on a call that could not store it.
	Produce func(context.Context) (map[string]string, error)
}

// SecretKeys writes the values opt.Produce answers into the keys of the
// Kubernetes Secret in the SOPS file, under stringData, in one sops write.
// Every check (the file is a Secret or absent with opt.New, the creation
// rule encrypts each key, no key is taken without opt.Replace) runs before
// the producer does. It answers each written key's length and keyed
// fingerprint, never a value.
func (o *Ops) SecretKeys(ctx context.Context, file string, opt SecretKeysOptions) ([]CaptureResult, error) {
	if len(opt.Keys) == 0 || opt.Produce == nil {
		return nil, errors.New("no keys or no producer")
	}
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) && opt.New == nil {
		return nil, fmt.Errorf("%s is absent: name the Secret it starts as", file)
	}
	if fi, err := os.Stat(filepath.Dir(file)); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s: no directory %s to write it in", file, filepath.Dir(file))
	}
	doc, err := o.document(ctx, file, opt.New)
	if err != nil {
		return nil, err
	}
	if k, _ := doc.get("kind"); k != secretKind {
		return nil, fmt.Errorf("%s holds no Kubernetes Secret", file)
	}
	for _, key := range opt.Keys {
		path, err := place(doc, file, key)
		if err != nil {
			return nil, err
		}
		_, inData := doc.get("data." + key)
		if _, inString := doc.get(path); (inString || inData) && !opt.Replace {
			return nil, fmt.Errorf("%s: the Secret holds %s already: replacing it takes --replace", file, key)
		}
	}
	values, err := opt.Produce(ctx)
	if err != nil {
		return nil, err
	}
	var out []CaptureResult
	for _, key := range opt.Keys {
		v, ok := values[key]
		if !ok || v == "" {
			continue
		}
		doc.remove("data." + key)
		path := doc.valuePath(key)
		if err := doc.set(path, v); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		out = append(out, CaptureResult{Key: Ref{File: file, Path: path}.String(), Bytes: len(v), Fingerprint: o.Fingerprint(v)})
	}
	if len(out) == 0 {
		return nil, errors.New("the producer answered none of the keys: nothing stored")
	}
	if err := o.encrypt(ctx, doc, file); err != nil {
		return nil, fmt.Errorf("the values are stored nowhere: %w", err)
	}
	return out, nil
}
