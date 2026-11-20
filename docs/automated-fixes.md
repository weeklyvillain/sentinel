# Automated fixes

Sentinel can hand an issue to Claude Code, which fixes it and opens a pull
request that says `Fixes #N`. A fix is asked for automatically for each new
issue of a service with **Auto-fix** on (Settings → Services, off by default),
when someone presses **Fix it** on an issue, or for every open error issue
at once with **Fix all open issues** on the Issues page (issues with a fix
queued, running or in an open pull request are skipped). Only error issues are sent,
not outages or performance issues. Claude Code runs in one of two places,
chosen under **Settings → Fixer** (`AUTOFIX_MODE` is the
default until you pick one there):

## Sentinel's fixer: the fixer container next to Sentinel

`sentinel-fixer` (`Dockerfile.fixer`, the `fixer` Compose service) runs the
official Claude Code CLI on the Sentinel server. It starts with
`docker compose up -d --build` and idles until there is work. Everything
else is set in the dashboard, under **Settings → Fixer**:

- **Claude sign-in**: your **Claude subscription** (run `claude setup-token`
  on your own machine and paste the token) or an Anthropic API key, billed
  per token. Fixes on a subscription count against your plan's usage
  limits, which assume ordinary individual use. The token is stored in
  Sentinel's database and only handed to the fixer with each job; the
  dashboard never shows it again. There is no "Sign in with Claude" button:
  Anthropic does not allow other apps to collect Claude logins.
- **Models and effort** for fixes and for investigations (default: Claude
  Code's). The model picker lists the models your sign-in can use (from the
  Anthropic Models API) plus the aliases (`opus`, `sonnet`, `haiku`, `fable`)
  that always pick the latest of a family; effort is Claude Code's
  `--effort` (`low` … `max`).
- **Commands Claude may run** in fixes, one per line, each with any
  arguments: `go test` allows `go test ./...`. The default is `go build`,
  `go test`, `go vet` and `gofmt`; shell syntax (`;`, `&&`, `|`, `$`) is
  refused. The fixer image has Go (with `CGO_ENABLED=0`; a newer Go from
  `go.mod` is downloaded) and Node; add other toolchains to
  `Dockerfile.fixer`. Investigations only ever read.
- **What Claude may do in fixes**: only those commands (default), **auto
  mode** (Claude Code's `--permission-mode auto`: it runs what the fix needs
  and a safety check blocks risky actions) or **everything** (no checks).
  The error report is telemetry, which users or attackers can influence, so
  with auto mode or everything a crafted report could make Claude run
  commands in the fixer container (network access, the Claude sign-in in
  its environment). It still cannot read Sentinel's or GitHub's tokens, and
  changes under `.github/` are never pushed.

Sentinel and the fixer share a generated token through the `fixer-auth`
volume, so neither needs `FIXER_TOKEN`. Environment variables still work as
defaults: `CLAUDE_CODE_OAUTH_TOKEN` / `FIXER_ANTHROPIC_API_KEY` sign the
fixer in when the dashboard has no sign-in, `FIXER_MODEL` /
`FIXER_ANALYZE_MODEL` are used when no model is set there, and a
`FIXER_TOKEN` in `deploy/.env` replaces the generated token for both. Test
`FIXER_EXTRA_TOOLS` (e.g. `Bash(make test:*)`) adds commands for every
fix on top of the dashboard's list.

The GitHub App also needs **Contents** and **Pull requests** (read and write);
accept the new permissions on each installation. For each job:

1. The fixer asks Sentinel for work (`POST /fixer/claim`, bearer the
   shared token) and gets the error report (samples, stack traces, tags,
   breadcrumbs) and a token that can push to **that one repository** for an
   hour.
2. It clones the repository and runs `claude -p` on a new branch. Claude may
   read and edit files and run read-only git, plus the commands in
   `FIXER_EXTRA_TOOLS`. It runs as a separate user without the fixer's or
   GitHub's tokens, and is told to treat the report as data. Add the
   toolchains your tests need to `Dockerfile.fixer`.
3. If Claude made a fix, the fixer commits it, pushes the branch and opens
   the pull request; changes under `.github/` are never pushed. Otherwise
   Claude's explanation is posted on the issue.
4. Sentinel follows the pull request as described under *GitHub Actions* below. A job without a result after
   an hour counts as failed and can be retried.

Settings → Fixer shows whether the fixer is connected, how it signs in and
what it is working on; it gets no work while it has no Claude sign-in.
**Issues → Fixer** lists every run (fixes and investigations) with its
result, model, turns, tokens and the cost Claude Code reports (on a
subscription that is the API-equivalent, not a bill), plus totals for the
last day, week and month. A run's page shows its log as it works: the
fixer's steps, Claude's messages and each tool call with its result, with
the repository and Claude tokens redacted. Logs are kept for 30 days, the
runs for a year.
`FIXER_TIMEOUT` (30m) and `FIXER_MAX_TURNS` (60) tune a run.

## Fixes across repositories

An error often involves more than one service: a frontend sending what the
API rejects, a caller and the service it calls. When the error's traces
show other services with a repository (Settings → Services), the fix job
brings up to three of those repositories along: the fixer clones them side
by side in one workspace, tells Claude which folder is which service, and
lets it change whichever need it (both sides of a contract, for example).

- Every changed repository gets a pull request on the same branch. The
  first says `Fixes owner/repo#N` (closing the issue), the others say they
  are part of that fix; once all are open, each one's description lists the
  others ("Merge together").
- Nothing is pushed if one of the repositories needs a change under
  `.github/`, which the fixer never pushes: half a fix is worse than none.
- The issue gets one comment with all pull requests; the issue page lists
  them with their state, and the fix counts as merged only when every one
  is merged (a closed one marks it closed). The other repositories' pull
  requests mention the issue, so Sentinel follows them on its timeline.
- Each repository gets its own push token, scoped to that repository; the
  GitHub App must be installed on all of them with Contents and Pull
  requests write.

## Investigations: is it worth fixing?

The same container can also do the AI analysis that writes each issue, in
place of the Anthropic API (Haiku, billed per token): choose **Claude Code in
the fixer** under Settings → AI (provider `claude-code`; it uses the
fixer's Claude sign-in, and fixes can still run in GitHub Actions). For every new error the
fixer clones the service's repository with a read-only token (Contents read
is enough), and Claude Code reads the code the stack trace points at, writes
the issue's title, summary, likely cause and next steps, and gives a verdict:

- **fix**: a bug in this code worth fixing. Only these get an automatic fix.
- **ignore**: noise (expected errors, wrong log level, someone else's outage
  this code already handles). With **Don't file issues for errors judged not
  worth fixing** on, these are ignored instead of filed.
- **investigate**: real, but not fixable in this code alone (infrastructure,
  configuration, capacity, data).

The verdict, severity and reason are in the issue ("Worth fixing?") and on
the Issues page. Investigations only read: `Read`, `Glob`, `Grep` and
`git log/show/blame`. They use `FIXER_ANALYZE_MAX_TURNS` (25) and the
investigation model set in the dashboard (or `FIXER_ANALYZE_MODEL`). The fix job later gets the
investigation along. When the fixer does not pick an error up within
20 minutes (or does not finish within 45), its issue is filed with the
template so nothing waits forever.

## GitHub Actions (default): a workflow in each repository

Sentinel adds the `AUTOFIX_LABEL` label (default `sentinel-autofix`) to the
GitHub issue and Claude Code runs in the repository's GitHub Actions:

1. The repository's workflow reacts to the label: Claude Code reads the
   issue, fixes the error, runs the tests and opens a pull request with
   `Fixes #N`. When it finds no confident fix it comments on the issue
   instead. Get the workflow from **Settings → Fixer**,
   with this Sentinel's label and GitHub App bot filled in, and save it as
   `.github/workflows/sentinel-autofix.yml`. The repository needs the secret
   `ANTHROPIC_API_KEY` (or a `claude_code_oauth_token`).
2. Sentinel follows the issue's timeline and shows the fix pull request on
   the issue (`fix.requested`, `fix.opened` and `fix.merged` webhooks).
   Merging it closes the issue, which resolves the group. If no pull request
   appears within 2 hours the attempt shows as "no fix PR"; **Try fixing
   again** starts another. If the error comes back after a merged fix and
   Auto-fix is on, Sentinel asks again with the regression.

The GitHub App needs no extra permissions for this (labels and the timeline
are part of Issues). Pull requests opened with the workflow's own token do not
start CI; give the step a GitHub App or fine-grained token as `GH_TOKEN` if
CI must run on them.
