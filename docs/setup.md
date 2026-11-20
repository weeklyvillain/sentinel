# First start and sign-in

## First start: Sentinel sets itself up

There is no GitHub App to create by hand and no key to copy. On first start
Sentinel prints a one-time **setup code** in its log; open the dashboard and:

1. **Enter the setup code** (`docker compose logs sentinel | grep "setup code"`).
   It proves you can read the server's log, so nobody else can claim a fresh
   install.
2. **Create Sentinel's GitHub App** on your account or an organization. Sentinel
   uses GitHub's [manifest flow](https://docs.github.com/apps/sharing-github-apps/registering-a-github-app-from-a-manifest):
   GitHub opens with the app filled in (Issues, Contents and Pull requests
   read and write, Metadata and organization Members read, callback and setup
   URLs, no webhook), you press **Create GitHub App**, and GitHub hands the
   app's ID, private key and OAuth credentials back to Sentinel, which keeps
   them in its database and restarts itself to use them.
3. **Install it and sign in with GitHub.** The first person to sign in from the
   browser that entered the code becomes the admin; the code is then gone.

Afterwards everything is in the dashboard: **Settings → Access** (who may sign
in: GitHub users and an organization, the ingest token, dry run, the GitHub
App), **Settings → GitHub** (installations and repositories), **Settings →
Fixer**, **Settings → AI**. **Setup** in the sidebar lists what is left.

## Signing in

Sign-in is always with GitHub. Only the users and organization members
allowed under Settings → Access (plus `ALLOWED_USERS` / `ALLOWED_ORG` from
the environment, if set) get in; an empty list lets nobody in. Saving a list
without yourself on it is refused, so you cannot lock yourself out.
Sessions are HttpOnly cookies (7 days). State-changing API calls require an
`X-Sentinel-Request` header, which cross-site pages cannot send.
`DASHBOARD_AUTH=none` disables sign-in for local development and only works
when the dashboard listens on localhost.

## Repositories

In the dashboard: **Settings → GitHub → Install on GitHub**, choose
repositories, come back, and switch on the repositories Sentinel may file
issues in. Then set a default repository and/or a repository per service.

An existing GitHub App still works from the environment (`GITHUB_APP_ID`,
`GITHUB_APP_PRIVATE_KEY_FILE`, `GITHUB_APP_CLIENT_ID`,
`GITHUB_APP_CLIENT_SECRET`); it wins over the one created in the dashboard.

Issues are routed: service repository → dashboard default → `GITHUB_REPO`.
With the app, the repository must be visible to an installation *and*
authorized in the dashboard; otherwise the group stays pending and the Issues
page says why. Installation tokens are short-lived and minted on demand.

`GITHUB_TOKEN` + `GITHUB_REPO` (a personal access token) still work when no
app is configured; the dashboard then cannot restrict repositories.

## Setup guide

**Setup** in the sidebar walks through everything above in order (dashboard
sign-in, telemetry, GitHub, repositories, services, dry run, AI, fixes,
Auto-fix, webhooks, uptime checks). Each step checks itself against what is
configured, for example whether the GitHub App's installations grant the
fixer Contents and Pull requests write, and says what is missing and where
to set it. Optional steps can be skipped; the sidebar counts the rest.
