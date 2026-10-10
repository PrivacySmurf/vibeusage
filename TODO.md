# TODO

## In Progress

## Up Next
- [ ] Confirm Codex renews unattended at its next natural expiry (access token expires 2026-10-20 22:22Z): `last_refresh` in `~/.codex/auth.json` advances, no `refresh_token_reused` in `~/.codex/logs_2.sqlite`, quota runner stays live. Until DSH covers Codex, do not re-import Codex in Settings → Everything OAuth: import overwrites `openai-codex` with the CLI chain, which DSH would rotate again. Another sharer to watch: warp_custom `openai-codex-gateway.mjs` runs Codex on copies of `auth.json` and writes refreshed copies back (idle since 2026-09-30 per its trace log).
- [ ] vibeusage: give Codex the Claude renewal parity — coordination dir (`~/.cache/vibeusage/codex-refresh`) with flock and `last-attempt.json` cooldown/backoff; classify `refresh_token_reused` / "refresh token was already used" as refresh-rejected (login required, no `codex exec` every runner cycle); add `vibeusage auth codex --diagnose` (access-token JWT expiry, `last_refresh`, binary, last outcome).
- [ ] DSH Codex models are broken (verified 2026-10-10): DSH's stored `openai-codex` copy returns 401 `token_revoked` from the ChatGPT usage endpoint, while the CLI's new login returns 200, and DSH has no other Codex path. Stopgap: an in-app Codex login in Settings → Everything OAuth (a separate DSH-owned chain); then re-check that `vibeusage --no-cache usage codex` still succeeds. DSH's Grok copy (expired 2026-10-08) is untested.
- [ ] Extend DSH Everything OAuth's CLI-owned contract (local.12 covers Claude only) to Codex and other CLI imports (the `grok-oauth` route has no owner either; no `~/.grok/auth.json` exists today): owner `codex-cli`, no pi-ai refresh of imported chains, read `~/.codex/auth.json` on use, expiry from the access-token JWT `exp` instead of import + 1 h, CLI-mediated renewal under a lock shared with vibeusage. Also a DSH bug: `runCoordinatedRenewal` writes `last-attempt.json` before `finish()` sets `duration_ms`, so DSH-written records always show 0 ms.
- [ ] Claude: observe a renewal started by the quota runner itself (its `binary_path` would be `~/.local/bin/claude`). Low priority: an unattended CLI-owned renewal is already confirmed (see Completed), and whichever actor reaches expiry first renews under the same shared lock.
- [ ] Runner-side CLI renewals depend on `~/.local/bin/claude` and `~/.local/bin/codex`, which symlink into nvm's node v24.13.0; removing that node version silently breaks them (`binary_missing`). Repoint them at a version-independent path, or add them to a drift check.
- [ ] Cosmetic: MiMo snapshots carry no `source`, so the quota runner labels live MiMo data "cache" (2026-10-10: snapshot fresh, live `--no-cache` fetch succeeds).

## Completed
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
