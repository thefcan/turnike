# turnike — project rules
- Go 1.22+, stdlib-first: net/http + httputil.ReverseProxy. No web frameworks, no third-party rate-limit libraries. Redis via go-redis only.
- Before every commit: `go test -race ./...` and `golangci-lint run` must be green. Limiter logic is pure and clock-injected (deterministic tests).
- No fabricated numbers: any figure in README or bench docs must come from a run executed in this repo; raw outputs live in bench/.
- Conventional commits, one per meaningful step. Ask before adding any new dependency.
- PROGRESS.md is session state: read it at session start, update it before session end.
- Milestones ship ONLY via the /ship-milestone skill; the advisor subagent reviews before every milestone commit.
