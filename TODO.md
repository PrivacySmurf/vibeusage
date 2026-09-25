# TODO

## In Progress

## Up Next

## Completed
- [x] Fix AppleScript sheet detection/click and throttle expired Model Studio sessions to avoid repeated failed queries (2026-09-24)
- [x] Add CLI commands and integration for ModelStudio provider with CDP cookie extraction and Keychain bypass (2026-09-24)
- [x] Install `just`, `golangci-lint`, and `pre-commit` locally and fix everything `just check` found (EOF/import-order, errcheck, govet, 6x staticcheck ST1005, 2 outdated GitHub Action pins) — `just check` now passes clean end-to-end (2026-09-24)
- [x] Clear the provider's throttle marker on successful `auth` (manual key, detected-credential reuse, device/custom flows, `--token`), so a stale pre-auth 429 cooldown can no longer outlive a successful re-authentication (2026-09-24)
- [x] Investigate Model Studio session lifecycle and automation constraints (2026-08-18)
- [x] Fix reset time parsing and effective reset calculations for expired period timestamps (2026-08-13)
- [x] Implement ModelStudio provider with browser cookie extraction and unit tests (2026-08-13)
- [x] Refactor Claude OAuth flow and update auth documentation (2026-08-13)
- [x] Support AGY CLI quota integration for weekly/session usage (2026-08-13)
- [x] Fix Codex weekly window classification (2026-08-13)
- [x] Fix Grok OAuth device flow (2026-08-13)
