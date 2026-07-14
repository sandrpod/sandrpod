# Contributing to SandrPod

Thank you for your interest in contributing! This document explains how to get started.

## Getting started

1. **Fork** the repository and clone your fork locally.
2. Install [Go](https://go.dev/dl/) (see `go.mod` for the minimum version).
3. Install [Docker](https://docs.docker.com/get-docker/) if you want to test the Poder layer.

```bash
git clone https://github.com/YOUR_USERNAME/sandrpod.git
cd sandrpod
go build ./...
go test ./...
```

## Development workflow

```bash
# Build all binaries
go build -o server  ./cmd/server
go build -o poder   ./cmd/poder
go build -o agent   ./cmd/agent
go build -o toolbox ./cmd/toolbox

# Run tests with race detector
go test -race ./...

# Vet
go vet ./...
```

## Making changes

1. Create a feature branch: `git checkout -b feat/my-feature`
2. Write tests before implementing (TDD encouraged).
3. Keep commits small and focused.
4. Follow [Conventional Commits](https://www.conventionalcommits.org/) for commit messages:
   - `feat:` new feature
   - `fix:` bug fix
   - `refactor:` code change that neither fixes a bug nor adds a feature
   - `docs:` documentation only
   - `test:` adding or fixing tests
   - `chore:` build/tooling changes

## Submitting a pull request

1. Ensure `go build ./...`, `go vet ./...`, and `go test ./...` all pass.
2. Update `CHANGELOG.md` under **Unreleased** for user-visible changes.
3. Open a PR against `main` using the provided PR template.
4. Address review feedback promptly.

## Code style

- Run `gofmt` / `goimports` on all Go files.
- No hardcoded secrets, PII, or machine-local paths in committed files.
- Keep functions under 50 lines and files under 800 lines where practical.

## Reporting security issues

Please **do not** open a public GitHub issue for security vulnerabilities.
Follow the process in [SECURITY.md](SECURITY.md) (private vulnerability
reporting via GitHub Security Advisories).

## License

SandrPod does not require a CLA. Contributions are accepted under the
"inbound = outbound" model: by contributing you agree that your contributions
will be licensed under the [Apache 2.0 License](LICENSE).
