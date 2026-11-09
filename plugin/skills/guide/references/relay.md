# guide — the relay, the watch and a first guide

The guide reads this at `GUIDE RELAY DUE`, in a headless turn, and as a first guide.

- **Hand over at `guide.relayAt`.** At `GUIDE RELAY DUE` the guide runs `beekeeper guide relay`
  (still over it `guide.relayGrace` later, the standby watch relays it, `GUIDE RELAYED`):
  beekeeper starts "Guide run N+1" as a fresh session and opens the relay to it. Its headless first
  turn takes the role with `beekeeper guide start`, checks owned work with `beekeeper guide work check` and ends. Once that turn ended, the relay prints
  one SendMessage to the successor's `local_` id: the guide makes exactly that call, which has the
  desktop start the successor's CLI, where it reads `beekeeper guide handover --prompt` (including every configured provider's subscription usage against pace and reset), arms `beekeeper guide watch` and
  acknowledges by name. What is not in beekeeper goes to it by `SendMessage` after that. Once
  `beekeeper guide status` exits 4 here, the guide stops its Monitor, leaves nothing running and
  ends. A relay not taken expires and the guide carries on. After a crash the standby unit starts
  the next run the same way. `beekeeper guide stop` runs only on the person's word.
- **Under `agents.mode: background`** the successor is a background session with Remote Control
  that takes the role and runs `beekeeper guide handover --prompt` itself: the relay prints no
  SendMessage to make, and its watch runs in that session. The desktop paragraphs here do not apply.
- **The watch runs in a desktop turn only.** A headless turn (a first turn, an `agents wake`) that
  arms a Monitor never ends, so the desktop never gets the session's CLI. The one exception is the standby
  watch's headless resume, whose message says the desktop runs no CLI of the session: that turn
  keeps the watch.
- **Only beekeeper starts a guide run**: the relay, the standby watch after a crash, `beekeeper
  guide start --new` (the person's "start the guide", from a terminal) and `beekeeper resume` after
  a stop and a reboot. Its first turn runs `beekeeper guide start` (`--take-over` only for a
  predecessor whose session still runs without a relay), arms `beekeeper guide watch`, and reads
  `beekeeper guide queue --full`. A session started any other way (`claude` by hand) is refused
  the role: never start one by hand; `beekeeper guide start --new` is the way.
