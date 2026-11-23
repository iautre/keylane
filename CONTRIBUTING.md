# Contributing to KeyLane

Thanks for considering a contribution.

## Before opening a pull request

- For a feature or behavior change, open an issue first so we can agree on scope.
- Keep changes focused and explain the user-visible effect in the pull request.
- Never include API keys, `.env` files, SQLite databases, request payloads, or provider credentials.
- Avoid adding dependencies unless the change needs them; explain the trade-off in the pull request.

## Local build checks

```sh
gofmt -w cmd internal
go build ./cmd/keylane
cd web
npm ci
npm run build
```

For local Go startup, copy `.env.example` to `.env`, set secrets, and export the file before starting the process. `DATABASE_ENABLED=false` must be present before Go package initialization because Gowk reads it during startup.

## Pull request notes

Include a short summary, how the change was checked, and any deployment or configuration impact. Do not claim checks that were not run. Keep documentation and `.env.example` in sync with configuration changes.
