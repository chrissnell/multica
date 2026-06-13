# GitHub App: multica-release-bot

## Why an App, not a PAT

A PAT inherits the developer's identity and pollutes commit history. The App
gets its own identity (`multica-release-bot[bot]`), short-lived tokens minted
per workflow run, and scoped permissions.

## Create the App

1. `https://github.com/settings/apps/new` — Create as a personal app.
2. Name: `multica-release-bot`.
3. Homepage URL: `https://github.com/chrissnell/multica`.
4. **Webhooks: disable** (we don't need them).
5. Permissions:
   - Repository: `Contents` → Read & Write (push commits, push tags).
   - Repository: `Pull requests` → Read-only (preflight reads PR bodies).
   - Repository: `Actions` → Read-only.
   - Account: (none).
6. Where can this app be installed: **Only on this account**.
7. Save. On the next page, click **Generate a private key**. Download the `.pem`.
8. Record the **App ID** shown at the top of the App settings page.
9. **Install App**: install on `chrissnell/multica` only (Only select repositories).
10. After install, record the **Installation ID** from the URL bar:
    `https://github.com/settings/installations/<INSTALLATION_ID>`.

## Wire into GitHub Actions

In `Settings → Secrets and variables → Actions`, add:
- `RELEASE_BOT_APP_ID` → the App ID from step 8.
- `RELEASE_BOT_PRIVATE_KEY` → the full contents of the `.pem` (multi-line).

These are consumed by `actions/create-github-app-token@v1` in
`.github/workflows/image-release.yml`.

## Wire into ARC

The runner scale set's secret (`arc-runners/multica-arc-github-app`,
created in `cluster/arc-install.md`) uses the same App credentials. Verify:

```bash
kubectl -n arc-runners get secret multica-arc-github-app
```

The secret has three keys: `github_app_id`, `github_app_installation_id`,
`github_app_private_key`.
