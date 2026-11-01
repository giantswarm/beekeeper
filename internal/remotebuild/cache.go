package remotebuild

import (
	"errors"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

const cacheTimeout = "5s"

// Cache configures optional shared storage. Credentials and endpoints are
// projected from a namespace Secret; configuration and pod specs hold no values.
type Cache struct {
	Enabled    bool   `yaml:"enabled"`
	Secret     string `yaml:"secret"`
	GoHelper   string `yaml:"goHelper"`
	GoPrefix   string `yaml:"goPrefix"`
	RustPrefix string `yaml:"rustPrefix"`
	ReadOnly   bool   `yaml:"readOnly"`
}

func (c Cache) defaults() Cache {
	if c.GoHelper == "" {
		c.GoHelper = "cacheprog"
	}
	if c.GoPrefix == "" {
		c.GoPrefix = "go/v1/"
	}
	if c.RustPrefix == "" {
		c.RustPrefix = "rust/v1/"
	}
	return c
}
func (c Cache) validate() error {
	if !c.Enabled {
		return nil
	}
	c = c.defaults()
	if c.Secret == "" || len(validation.IsDNS1123Subdomain(c.Secret)) != 0 {
		return errors.New("run.remote.cache.secret must name a namespace Secret")
	}
	if strings.ContainsAny(c.GoHelper, " \t\r\n\x00'\"`$") || (!strings.HasPrefix(c.GoHelper, "/") && strings.Contains(c.GoHelper, "/")) {
		return errors.New("run.remote.cache.goHelper must be an image executable name or absolute path")
	}
	for _, prefix := range []string{c.GoPrefix, c.RustPrefix} {
		if strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "\r\n\x00\\") || path.Clean(prefix) == ".." || strings.HasPrefix(path.Clean(prefix), "../") {
			return errors.New("run.remote.cache prefixes must be relative object prefixes")
		}
	}
	if strings.TrimSuffix(c.GoPrefix, "/") == strings.TrimSuffix(c.RustPrefix, "/") {
		return errors.New("run.remote.cache Go and Rust prefixes must differ")
	}
	return nil
}

// Only known storage keys are projected: no envFrom can override PATH or
// activate debugging that could expose credential-bearing requests.
func (c Cache) env() []corev1.EnvVar {
	if !c.Enabled {
		return nil
	}
	c = c.defaults()
	mode := "READ_WRITE"
	readOnly := "false"
	if c.ReadOnly {
		mode = "READ_ONLY"
		readOnly = "true"
	}
	env := []corev1.EnvVar{
		{Name: "BEEKEEPER_CACHE_ENABLED", Value: "1"},
		{Name: "BEEKEEPER_GO_CACHE_HELPER", Value: c.GoHelper},
		{Name: "CACHEPROG_REMOTE_STORAGE_TYPE", Value: "s3"},
		{Name: "CACHEPROG_S3_FORCE_PATH_STYLE", Value: "true"},
		{Name: "CACHEPROG_S3_PREFIX", Value: c.GoPrefix},
		{Name: "CACHEPROG_DISABLE_PUT", Value: readOnly},
		{Name: "CACHEPROG_REMOTE_GET_TIMEOUT", Value: cacheTimeout},
		{Name: "CACHEPROG_REMOTE_PUT_TIMEOUT", Value: cacheTimeout},
		{Name: "CACHEPROG_REMOTE_STORAGE_MAX_CONSECUTIVE_ERRORS", Value: "3"},
		{Name: "CACHEPROG_REMOTE_STORAGE_RETRY_AFTER", Value: "0s"},
		{Name: "CACHEPROG_MAX_BACKGROUND_WAIT", Value: cacheTimeout},
		{Name: "CACHEPROG_MAX_CONCURRENT_REMOTE_GETS", Value: "4"},
		{Name: "CACHEPROG_MAX_CONCURRENT_REMOTE_PUTS", Value: "4"},
		{Name: "CARGO_INCREMENTAL", Value: "0"},
		{Name: "SCCACHE_S3_KEY_PREFIX", Value: c.RustPrefix},
		{Name: "SCCACHE_S3_RW_MODE", Value: mode},
		{Name: "SCCACHE_IGNORE_SERVER_IO_ERROR", Value: "1"},
		{Name: "SCCACHE_IDLE_TIMEOUT", Value: "60"},
	}
	for _, key := range []string{"CACHEPROG_S3_ENDPOINT", "CACHEPROG_S3_BUCKET", "CACHEPROG_S3_REGION", "CACHEPROG_S3_ACCESS_KEY_ID", "CACHEPROG_S3_ACCESS_KEY_SECRET", "CACHEPROG_S3_SESSION_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION", "AWS_ENDPOINT_URL_S3", "SCCACHE_BUCKET", "SCCACHE_ENDPOINT", "SCCACHE_REGION", "SCCACHE_S3_USE_SSL"} {
		env = append(env, corev1.EnvVar{Name: key, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: c.Secret}, Key: key, Optional: ptr.To(true)}}})
	}
	return env
}

// cacheSetup runs before the command, never retries the command, and emits only
// numeric cache statistics. Provider diagnostics may include credential-bearing
// requests, so they never enter the run journal.
const cacheSetup = `cache_rust_started=0
if [ "${BEEKEEPER_CACHE_ENABLED:-}" = 1 ]; then
 case "$tool" in
 go)
  export CACHEPROG_ROOT_DIRECTORY="$r/cache"
  if [ -n "${CACHEPROG_S3_BUCKET:-}" ] && [ -n "${CACHEPROG_S3_ACCESS_KEY_ID:-}" ] && [ -n "${CACHEPROG_S3_ACCESS_KEY_SECRET:-}" ] && command -v "${BEEKEEPER_GO_CACHE_HELPER:-cacheprog}" >/dev/null 2>&1 && timeout --foreground -k 1s 15s "$BEEKEEPER_GO_CACHE_HELPER" direct --root-directory "$CACHEPROG_ROOT_DIRECTORY" </dev/null >/dev/null 2>&1; then
   export BEEKEEPER_CACHE_LOG="$r/log"
   cat > "$r/go-cache" <<'CACHEGO'
#!/bin/bash
export CACHEPROG_ROOT_DIRECTORY="$CACHEPROG_ROOT_DIRECTORY/$$"
exec 3> >(warned=0; while IFS= read -r line; do
 if [ "$warned" = 0 ] && [[ "$line" == *"Remote storage has been disabled because of too many consecutive errors"* ]]; then
  printf 'beekeeper run: Go shared cache unavailable; using local cache\n' >> "$BEEKEEPER_CACHE_LOG"; warned=1
 fi
 if [[ "$line" == *"cacheprog statistics"* ]] && [[ "$line" =~ get_hits[\"=:[:space:]]+([0-9]+) ]]; then
  printf 'beekeeper run: Go cache hits %s\n' "${BASH_REMATCH[1]}" >> "$BEEKEEPER_CACHE_LOG"
 fi
done)
filter=$!
"$BEEKEEPER_GO_CACHE_HELPER" direct --root-directory "$CACHEPROG_ROOT_DIRECTORY" 2>&3
rc=$?
exec 3>&-
wait "$filter"
exit "$rc"
CACHEGO
   chmod 700 "$r/go-cache"
   export GOCACHEPROG="$r/go-cache"
  else
   cache_start_status=$?
   unset GOCACHEPROG
   printf 'beekeeper run: shared Go cache unavailable (startup status %s); using pod-local cache\n' "$cache_start_status" >> "$r/log"
  fi
  ;;
 cargo)
  export SCCACHE_DIR=/work/sccache
  if [ -n "${SCCACHE_BUCKET:-}" ] && [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] && command -v sccache >/dev/null 2>&1; then
   cache_rust_started=1
   if timeout --foreground -k 1s 15s sccache --start-server >/dev/null 2>&1; then
    cache_rust_started=2
    export RUSTC_WRAPPER=sccache
   else
    cache_start_status=$?
    unset RUSTC_WRAPPER
    printf 'beekeeper run: shared Rust cache unavailable (startup status %s); using compiler directly\n' "$cache_start_status" >> "$r/log"
   fi
  else
   unset RUSTC_WRAPPER
   printf 'beekeeper run: shared Rust cache unavailable; using compiler directly\n' >> "$r/log"
  fi
  ;;
 esac
fi
`

const cacheFinish = `if [ "${cache_rust_started:-0}" = 2 ]; then
 timeout --foreground -k 1s 5s sccache --show-stats 2>/dev/null | while read -r a b n d rest; do
  case "$a $b" in
   'Cache hits'|'Cache misses'|'Cache writes'|'Non-cacheable calls')
    if [[ "$n" =~ ^[0-9]+$ ]]; then printf 'beekeeper run: Rust %s %s %s\n' "${a,,}" "${b,,}" "$n" >> "$r/log"; fi;;
   'Cache write')
    if [ "$n" = errors ] && [[ "$d" =~ ^[0-9]+$ ]]; then printf 'beekeeper run: Rust cache write errors %s\n' "$d" >> "$r/log"; fi;;
  esac
 done
fi
if [ "${cache_rust_started:-0}" != 0 ]; then
 timeout --foreground -k 1s 5s sccache --stop-server >/dev/null 2>&1 || true
fi
rm -rf "$r/cache"
`
