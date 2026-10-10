# TODO

## In Progress

## Up Next
- [ ] Confirm Codex renews unattended at its next natural expiry (access token expires 2026-10-20 22:22Z): `last_refresh` in `~/.codex/auth.json` advances, no `refresh_token_reused` in `~/.codex/logs_2.sqlite`, `~/.cache/vibeusage/codex-refresh/last-attempt.json` (if written) shows `refreshed` with a real `duration_ms`, quota runner and DSH `codex-oauth` stay live. Re-importing Codex in DSH is safe since local.13 (import records `codex-cli` and stores no refresh token). Another sharer to watch: warp_custom `openai-codex-gateway.mjs` runs Codex on copies of `auth.json` and writes refreshed copies back (idle since 2026-09-30 per its trace log).
- [ ] DSH Everything OAuth follow-ups (local.13 covers Claude and Codex): the `grok-oauth` route still has no owner and DSH's Grok copies (`xai`, `grok-oauth`; expired 2026-09-28/10-08) are untested — no `~/.grok/auth.json` exists today, and vibeusage's own Grok chain is separate. `discover.ts` import still reads a fixed `~/.codex/auth.json` (live resolution honors `CODEX_HOME`). Source branch `discreetbear/codex-cli-ownership-local13` in `$DSH_HOME/vendor/dsh-everything-oauth-source` is deployed but not merged to that repo's `main` (same as the local.12 lineage).
- [ ] Claude: observe a renewal started by the quota runner itself (its `binary_path` would be `~/.local/bin/claude`). Low priority: two unattended CLI-owned renewals are confirmed (2026-10-10 15:08:46Z and 23:03:53Z, both DSH-initiated), and whichever actor reaches expiry first renews under the same shared lock.
- [ ] Runner-side CLI renewals depend on `~/.local/bin/claude` and `~/.local/bin/codex`, which symlink into nvm's node v24.13.0; removing that node version silently breaks them (`binary_missing`). Repoint them at a version-independent path, or add them to a drift check.
- [ ] Cosmetic: MiMo snapshots carry no `source`, so the quota runner labels live MiMo data "cache" (2026-10-10: snapshot fresh, live `--no-cache` fetch succeeds).

## Completed
- [x] Ship DSH Everything OAuth 0.1.0-local.13 (commits 214192c/324cb08/ae17cc8, tarball sha256 474ce39a…): imported Codex chains are `codex-cli`-owned, read from the live CLI store on use, never refreshed by pi-ai, renewed only through `codex exec` under the shared `codex-refresh` lock; migration claimed `codex-oauth`, stripped the stored refresh token, and removed the stale alias; `duration_ms` now persisted. Deployed by ops job `20261010T233102Z-everything-oauth-local13` (host PID 77536). Verified live: status shows codex `owner=codex-cli source=auth-file expiresAt=2026-10-20T22:22:42Z`; a real `llm.stream` request on `codex-oauth`/gpt-6-luna returns "OK" (before: "invalidated oauth token"), and `claude-oauth`/claude-sonnet-5-5 still returns "OK" (2026-10-10)
- [x] vibeusage Codex renewal parity (commit 7891511): shared `~/.cache/vibeusage/codex-refresh` coordination, minimal `codex exec` renewal with `OPENAI_API_KEY`/`CODEX_API_KEY` stripped and a neutral cwd, JWT-derived expiry, `CODEX_HOME`-aware auth file, refresh-rejected/login-required hints, and `vibeusage auth codex --diagnose`; `just check` green on branch and merged main; reinstalled, live uncached fetch and runner OK (2026-10-10)
- [x] Fix DSH Codex models broken by the revoked `openai-codex` copy (401 `token_revoked`) — resolved by local.13 reading the CLI login (2026-10-10)
- [x] Re-authenticate Codex after the dead chain (`refresh_token_reused` since 2026-10-10 19:35Z; DSH Everything OAuth imported `~/.codex/auth.json` on 2026-10-01 with an assumed 1 h expiry, rotated it through pi-ai at 15:53Z, and kept the new chain only under `openai-codex`): user ran `codex login` 2026-10-10 22:22Z; new chain is distinct from both DSH copies, the DSH store is untouched, and live uncached `vibeusage usage codex` plus the quota runner succeed (2026-10-10)
- [x] Confirm Model Studio automatic sign-in renews the session unattended at natural expiry: the stored session was rejected, CDP auto-login ran at 2026-10-10 13:58Z (`modelstudio-login.json` marker) and saved a quota-validated session (`updated_at` now recorded); later runner fetches stay live and uncached (2026-10-10)
- [x] Confirm Claude CLI-owned unattended renewal at natural expiry: 2026-10-10 15:08:46Z, DSH Everything OAuth local.12 launched the real `claude` CLI under vibeusage's shared `claude-refresh` lock, the CLI rotated its own chain (new Keychain token, expires 23:08:48Z), and DSH stopped it with SIGTERM (exit 143) (2026-10-10)
- [x] Align DSH Everything OAuth with CLI-owned Claude credentials: local.12 (commit 93c3a64, 2026-10-09) is live since the 2026-10-09 14:37 local host restart — `claude-oauth` route owner `claude-cli`, stored refresh token retired, renewal CLI-mediated under the shared lock (verified 2026-10-10)
- [x] Re-authenticate Claude CLI and confirm live, uncached Claude usage after `claude auth login`; canonical current-user Keychain credentials selected (2026-10-09).
- [x] Bump the Go toolchain to 1.27.2 in go.mod (CI follows go-version-file); `just check` now passes, including `vuln` with zero called vulnerabilities (2026-10-09)
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
