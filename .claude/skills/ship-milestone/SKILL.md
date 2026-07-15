---
name: ship-milestone
description: Ship the current milestone — verify green, get advisor review, update PROGRESS.md, commit and push. Use at the end of every milestone (M0–M7).
---
Ship milestone $ARGUMENTS:
1. Run `go test -race ./...` and `golangci-lint run`. Anything red → fix first.
2. Delegate to the advisor subagent: review the full diff since the last milestone commit. If verdict is FIX FIRST, address the findings and repeat step 1.
3. Update PROGRESS.md: mark the milestone done; write "next action" + open decisions.
4. Conventional commit (e.g. "feat(limiter): M2 — token bucket, sliding window, fixed window") and push.
5. Print: milestone shipped · next action · decisions waiting on me.
