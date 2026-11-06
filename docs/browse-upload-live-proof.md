# Browse upload compatibility check

After upgrading Claude Code, run this check from the repository before
releasing or relying on `browse --file`:

```sh
make browse-upload-live
# Or directly:
go test -tags browselive -run '^TestBrowseUploadLive$' -count=1 -timeout=10m ./cmd
```

Use an environment with the installed, authenticated Claude CLI, connected
Chrome extension, and the normal beekeeper browse configuration and hooks.
The browser must be on the same machine as the test's local HTTP server.
The check uses the current configuration, builds the repository's binary,
and opens a temporary upload page. It does not publish an attachment.
Missing prerequisites fail the check; they are not treated as a skip.

The check shares a temporary file through the actual `browse --file` command
and requires the page's server to receive its exact random bytes. A successful
CLI exit or a report claiming success is insufficient. The payload is never
included in the prompt or page. The normal browse permissions remain in place,
including `--tools ''` and the upload-target guard.

`CLAUDE_STAGE_FILE_ROOT` is an undocumented Claude CLI interface. If a CLI
upgrade removes or renames it, file upload can fail with a Read restriction
even though the file was shared. Investigate the installed CLI's staging
contract when this check fails; do not grant Read access to make it pass.
The live check is opt-in because ordinary CI has no authenticated desktop
browser session.
