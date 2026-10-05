package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Setup is what [Ops.Setup] did: the shared vault, whether it created it,
// the service account and the length of its token, never the token.
type Setup struct {
	Vault          string `json:"vault"`
	VaultID        string `json:"vaultId"`
	VaultCreated   bool   `json:"vaultCreated"`
	ServiceAccount string `json:"serviceAccount"`
	TokenFile      string `json:"tokenFile"`
	TokenBytes     int    `json:"tokenBytes"`
}

// ErrSetUp is a token file that already holds a token: setup creates one
// service account once, a second would orphan the first.
var ErrSetUp = errors.New("already set up")

// Setup creates the shared vault when absent and a service account that
// reads and writes it, and writes the account's token to tokenFile (0600).
// It runs op as the person: the caller's own signed-in session, the one
// thing a service account cannot do. The token goes from op's stdout to the
// file and nowhere else.
func (o *Ops) Setup(ctx context.Context, account, tokenFile string) (Setup, error) {
	s := Setup{Vault: o.Vault, ServiceAccount: account, TokenFile: tokenFile}
	switch {
	case o.Vault == "":
		return s, fmt.Errorf("%w: no shared vault is configured (secret.vault)", ErrVault)
	case tokenFile == "":
		return s, fmt.Errorf("%w: no token file is configured (secret.tokenFile)", ErrVault)
	case account == "":
		return s, errors.New("name the service account")
	}
	if fi, err := os.Stat(tokenFile); err == nil && fi.Size() > 0 {
		return s, fmt.Errorf("%s: %w: it holds a token; a new service account needs the old one revoked and the file moved aside first", tokenFile, ErrSetUp)
	}
	id, created, err := o.ensureVault(ctx)
	if err != nil {
		return s, err
	}
	s.VaultID, s.VaultCreated = id, created
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		return s, err
	}
	tok, err := o.asPerson(ctx, nil, "service-account", "create", account,
		"--vault", o.Vault+":read_items,write_items", "--raw")
	if err != nil {
		return s, fmt.Errorf("%w: service account %s: %w", ErrVault, account, err)
	}
	t := strings.TrimSpace(string(tok))
	if t == "" {
		return s, fmt.Errorf("%w: service account %s: op answered no token", ErrVault, account)
	}
	if err := writeSecretFile(tokenFile, []byte(t+"\n")); err != nil {
		return s, fmt.Errorf("the service account %s exists, its token is not written: revoke it in 1Password: %w", account, err)
	}
	s.TokenBytes = len(t)
	return s, nil
}

// ensureVault answers the shared vault's ID, creating the vault when the
// person's session finds none (op's "isn't a vault"); any other failure,
// a signed-out session first, creates nothing.
func (o *Ops) ensureVault(ctx context.Context) (string, bool, error) {
	var v struct {
		ID string `json:"id"`
	}
	raw, err := o.asPerson(ctx, nil, "vault", "get", o.Vault, "--format", "json")
	created := false
	if err != nil && !strings.Contains(err.Error(), "isn't a vault") {
		return "", false, fmt.Errorf("%w: vault %s: %w", ErrVault, o.Vault, err)
	}
	if err != nil {
		if raw, err = o.asPerson(ctx, nil, "vault", "create", o.Vault, "--format", "json"); err != nil {
			return "", false, fmt.Errorf("%w: vault %s: %w", ErrVault, o.Vault, err)
		}
		created = true
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.ID == "" {
		return "", false, fmt.Errorf("%w: vault %s: op answered no vault", ErrVault, o.Vault)
	}
	return v.ID, created, nil
}

// Import copies one field of a vault the person reads, outside the shared
// vault, into a field of the shared vault (creating the item or the field
// when absent), and answers the value's length. The source is read with
// the person's session, the destination written as the service account.
func (o *Ops) Import(ctx context.Context, src, dst Ref) (int, error) {
	if src.Op == "" || dst.Op == "" {
		return 0, errors.New("import copies an op://<vault>/<item>/<field> into one of the shared vault")
	}
	if src.vault() == o.Vault {
		return 0, fmt.Errorf("%s: already in the shared vault: copy it, not import", src.Op)
	}
	if err := o.checkVault(dst); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	v, err := o.asPerson(ctx, nil, "read", "--no-newline", src.Op)
	if ctx.Err() != nil {
		return 0, fmt.Errorf("%w: %s: op answered nothing in %s", ErrVault, src.Op, opTimeout)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrVault, src.Op, err)
	}
	if len(v) == 0 {
		return 0, fmt.Errorf("%s: the field is empty", src.Op)
	}
	if err := o.storeVault(ctx, dst, string(v)); err != nil {
		return 0, err
	}
	return len(v), nil
}

// asPerson runs op in the caller's own environment, its signed-in session,
// without the service account's token.
func (o *Ops) asPerson(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return o.Run(ctx, "", nil, stdin, "op", args...)
}

// writeSecretFile creates path with mode 0600 from data, through a rename.
func writeSecretFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck,gosec // the write error wins
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
