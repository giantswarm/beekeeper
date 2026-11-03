# guide — the board and machine changes

The guide reads this before its daily board reconciliation and before a machine change the person
approves.

- **The board is reconciled daily.** Each worker moves its own item; once a day the guide reconciles
  the board's items against their live issues and PRs: status, blockers, sub-issues, the decisions
  taken. When the person tells the guide to change priorities or reorder work on the board, the guide
  changes the board order with `beekeeper board rank <item> --top | --before <item> | --after <item>`
  (or `beekeeper board order`) and `beekeeper board move <item> <status>`. The guide does no other
  reading of the board.
- **Machine changes the person approves are carried out here.** A unit, hook or other change to the
  machine that the person approves by typing it in the guide's session, the guide carries out
  itself as commands (the file tools are refused here): their typed word is in the session that
  acts, and no relay is involved.
