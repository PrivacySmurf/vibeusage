# TODO

## In Progress

## Up Next
- [ ] Confirm Model Studio automatic sign-in renews the session unattended at the next natural expiry (2026-10-08: unverified manually — no slider captcha appeared; `vibeusage auth modelstudio --diagnose` now makes the check a one-liner)
- [ ] Bump the Go toolchain: `just check` fails at `vuln` with 9 called stdlib findings — pre-existing (confirmed identical on origin/main `e282d10`), environmental drift between the installed Go and the vuln DB, untouched by local changes (2026-10-09)

## Completed
- [x] Model Studio: page-target cookie fallback for CDP import (Chrome 154+ rejects browser-level `Storage.getCookies`) + capture the SSO session while the sign-in tab is open — its cookies are scoped to the tab and vanish when it closes (2026-10-08)
- [x] Full provider automation: binary path resolution, MiMo CDP auto-login, Claude exponential backoff with CLI health probe (2026-10-03)
- [x] Add executil.ResolveBinary for fallback CLI discovery (~/.local/bin, /opt/homebrew/bin) — fixes Antigravity, Claude, Codex, Gemini (2026-10-03)
- [x] MiMo CDP auto-login via Xiaomi SSO — navigates to platform, imports cookies from active tab, no form-filling needed (2026-10-03)
- [x] Claude exponential backoff on consecutive 429s (60s→4h cap) with HealthProber CLI probe at ≥5 failures (2026-10-03)
- [x] Implement Xiaomi MiMo provider for balance and token plan usage with CDP and browser cookie auto-import (2026-10-01)
- [x] Scope Codex usage requests to the active ChatGPT account, expose stale-cache fetch failures, and update mise-action workflow pins (2026-09-29)
- [x] Fix post-reboot Claude and Model Studio credential state, extract active session cookies from Chrome CDP, and configure persistent launchd service for DSH Computer Use CUADaemon (2026-09-26)
- [x] Fix Claude OAuth 429 refresh handling, select Keychain credentials over stale file tokens, preserve authentic provider source in JSON output, and support non-invasive background CDP profiles for Model Studio (2026-09-26)
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
