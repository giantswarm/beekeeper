package config

import (
	"slices"
	"strings"
)

// Secret configures beekeeper secret, the credential operations beekeeper
// runs so that no agent reads a value.
type Secret struct {
	// Vault is the team's shared 1Password vault, the only one an op://
	// reference may name; unset, beekeeper secret reads SOPS files only.
	Vault string `yaml:"vault"`
	// Vaults are the shared vaults besides Vault that an op:// reference
	// is read from (copy, compare, fingerprint) through the same session
	// or service account, such as a team's shared vault; beekeeper writes
	// to Vault alone. The list is the person's policy.
	Vaults []string `yaml:"vaults"`
	// MoveVaults are the explicitly allowed destinations for whole-item moves.
	// They do not grant field writes or reads. Empty refuses every move.
	MoveVaults []string `yaml:"moveVaults"`
	// TokenFile (~/ allowed) holds the vault's service account token,
	// which beekeeper gives only to its own op calls. Agents reach it
	// nowhere: it lies outside every agent container's mounts.
	TokenFile string `yaml:"tokenFile"`
	// Session reads the vault through the person's own signed-in op
	// session instead of a service account (tokenFile then unused): for a
	// vault no service account can be granted, such as an Employee vault.
	// The broker signs in by itself (SigninCommand) and holds the session in
	// its memory alone; every call on an op:// reference runs there.
	Session bool `yaml:"session"`
	// UnlockCommands are the names of the person's own commands that sign
	// in to or unlock the vault (helpers around op signin): the hook
	// refuses them in agent sessions under any path, like op signin, and
	// the agent shell prelude removes their aliases and shell functions.
	UnlockCommands []string `yaml:"unlockCommands"`
	// FileRoots are the directories (~/ allowed) whose plain files beekeeper
	// secret reads one value of as a source, file://<absolute path>#<path>:
	// a CLI's own credential directory, for one. A file outside them is
	// refused; none configured refuses every file:// source.
	FileRoots []string `yaml:"fileRoots"`
	// Files are files known to hold secret values (~/ and globs allowed)
	// that the hook refuses to let an agent read whole, beside its built-in
	// list (omp's provider configuration and the like) and TokenFile.
	Files []string `yaml:"files"`
	// SigninCommand is the command the broker runs to sign in to the vault
	// without the person, when it starts and whenever a call waits while it
	// holds no session: it prints the session as op signin does
	// (export OP_SESSION_<id>="<token>") on stdout, its log on stderr. Empty
	// leaves the sign-in to the person's beekeeper secret unlock.
	SigninCommand []string `yaml:"signinCommand"`
	// UnlockWait is how long a call that needs the vault waits for the
	// broker's sign-in while it holds no session (8m, within the Bash tool's
	// 10 minutes).
	UnlockWait Duration `yaml:"unlockWait"`
	// SessionLifetime is how long the broker holds the session after the
	// person's unlock (12h): it keeps op's session from idling out until
	// then, and forgets it at the end.
	SessionLifetime Duration `yaml:"sessionLifetime"`
	// AgeIdentities are the age identities beekeeper reads from the shared
	// vault, an identity file or the person's own credential store for the
	// SOPS files that sops' own sources (SOPS_AGE_KEY, SOPS_AGE_KEY_FILE,
	// sops/age/keys.txt) hold none for: each read in beekeeper's process
	// (the broker's, with secret.session) and given to the one sops call
	// alone.
	AgeIdentities []AgeIdentity `yaml:"ageIdentities"`
	// AgeVaults are the 1Password vaults besides secret.vault that age
	// identities are read from, through the same session or service
	// account: an op:// entry of ageIdentities may name one, and a SOPS file
	// of installations/<name>/ no entry names finds its identity in the item
	// "<name>.agekey" of secret.vault or one of them.
	AgeVaults []string `yaml:"ageVaults"`
	// Store is the person's own credential store a store:// age identity
	// is read from.
	Store SecretStore `yaml:"store"`
	// Env are the names (globs, such as *_TOKEN) of the environment
	// variables that carry a secret: the producer of beekeeper secret
	// capture runs without them, as without the vault's credentials and
	// SOPS_AGE_KEY, which it never gets.
	Env []string `yaml:"env"`
}

// SecretStore reaches the person's own credential store through the
// person's own commands, run by the broker: beekeeper never handles the
// store's password, and the store shows whatever unlock prompt it shows.
type SecretStore struct {
	// Read prints the secret of the entry appended as its last argument on
	// stdout.
	Read []string `yaml:"read"`
	// Search prints the names of the entries matching the term appended as
	// its last argument (an age recipient), one per line, never a value.
	Search []string `yaml:"search"`
}

// AgeIdentity maps the SOPS files of an age recipient, or under a path, to
// the vault field or the identity file that holds the recipient's identity.
type AgeIdentity struct {
	// Recipient is the age recipient (age1…) as the files' sops metadata
	// and .sops.yaml name it.
	Recipient string `yaml:"recipient"`
	// PathRegex matches a file's absolute path (unanchored), for the files
	// of a repository or an installation whatever their recipient.
	PathRegex string `yaml:"pathRegex"`
	// Ref is the op:// field of the shared vault (secret.vault) or of one
	// of secret.ageVaults, the file:/// identity file (an absolute path, comments allowed) or the
	// store:// entry of the person's own credential store (secret.store;
	// store:// alone searches it for the file's recipients) holding the
	// identity, AGE-SECRET-KEY-1….
	Ref string `yaml:"ref"`
}

// Scan configures the transcript value scanner: beekeeper scan index
// fingerprints the values of these sources into the index under the state
// directory (scan/), which the PostToolUse hook and beekeeper scan sweep
// match against.
type Scan struct {
	// SOPS are globs (~/ allowed) of SOPS files whose values are indexed,
	// decrypted with the sops binary in beekeeper's own process.
	SOPS []string `yaml:"sops"`
	// Vaults are 1Password vaults whose concealed fields are indexed, read
	// with the op binary.
	Vaults []string `yaml:"vaults"`
	// MinLength is the shortest value indexed (default 12): shorter ones
	// would match ordinary output.
	MinLength int `yaml:"minLength"`
}

// StoreRule refuses a secret-store write (op item or document create and
// edit, vault kv put and patch) whose vault and item match its globs
// (case-insensitive); an empty glob matches any, as does a write that names
// no vault.
type StoreRule struct {
	Vault string `yaml:"vault"`
	Item  string `yaml:"item"`
}

// opRef reports whether s is an op:// reference of a field,
// op://<vault>/<item>/<field>, as beekeeper secret reads them.
func opRef(s string) bool {
	parts := strings.Split(strings.TrimPrefix(s, "op://"), "/")
	return strings.HasPrefix(s, "op://") && len(parts) >= 3 && !slices.Contains(parts, "")
}
