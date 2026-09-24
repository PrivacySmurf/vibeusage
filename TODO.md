# TODO

## In Progress

## Up Next
- [ ] Add CLI commands and integration for ModelStudio provider
- [ ] Clear/bypass the Claude throttle marker (`~/Library/Caches/vibeusage/throttles/claude.json`) on `vibeusage auth claude` re-login, so a stale pre-login 429 `retry_at` doesn't keep blocking fetches after credentials are refreshed

## Completed
- [x] Investigate Model Studio session lifecycle and automation constraints (2026-08-18)
- [x] Fix reset time parsing and effective reset calculations for expired period timestamps (2026-08-13)
- [x] Implement ModelStudio provider with browser cookie extraction and unit tests (2026-08-13)
- [x] Refactor Claude OAuth flow and update auth documentation (2026-08-13)
- [x] Support AGY CLI quota integration for weekly/session usage (2026-08-13)
- [x] Fix Codex weekly window classification (2026-08-13)
- [x] Fix Grok OAuth device flow (2026-08-13)
