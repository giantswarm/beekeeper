# Mid-turn Chrome disconnect replay

`go test ./cmd -run '^TestBrowseDisconnectCLI$' -count=1 -v` runs this fixture
in the normal test suite, including CI. It needs a POSIX shell, but no browser,
model, credentials, browser lease or network connection.

The compiled command test executable runs `Main` as a child process using
`BEEKEEPER_TEST_MAIN=1`, the same entry point and exit-code mapping as the CLI.
`claude.sh` replaces only the external Claude process on PATH. It appends a
turn to the requested session's transcript and prints that turn's report.
Each case has its own configuration, state and transcript directory.

These small, sanitized transcript fixtures reproduce the CLI's real
"Claude in Chrome is not connected" tool-result text and the empty `[]`
browser list. They are representative replay records, not a recording of a
forced disconnect against a live browser. Tool result envelopes, including
`is_error: false`, match the disconnect response. Browser identifiers, tab
identifiers and page content are fixture data.

The first turn lists a connected browser and navigates successfully before
losing the connection on `get_page_text`. The second turn either selects a
returned browser and retries the read, or finds no browser. Assertions cover
one reconnect with the same session and tool rules, the resume prompt, both
reports, the `connection lost:` diagnostic, exit 0 on recovery, and exit 1
with unfinished steps on persistent loss. A third Claude invocation is refused
and recorded, so an extra retry cannot pass unnoticed.
