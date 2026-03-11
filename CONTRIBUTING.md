# Contributing to sipgate-sip-stream-bridge

Thank you for your interest in contributing!

## Reporting Bugs

Please open a [GitHub Issue](../../issues) and include:
- A clear description of the problem
- Steps to reproduce
- Expected vs. actual behaviour
- Environment details (OS, Docker version, Go/Node version)

## Proposing Features

Open an issue first to discuss the idea before investing time in an implementation. This avoids duplicate work and ensures the feature aligns with the project direction.

## Pull Requests

1. Fork the repository and create a branch from `main`
2. Make your changes — keep PRs focused and small
3. Ensure existing tests pass and add tests for new behaviour
4. Run linters (see below)
5. Open a PR against `main` with a clear description of the change

## Development Setup

See the implementation-specific READMEs:
- [Go implementation](./go/README.md)
- [Node.js implementation](./node/README.md)

## Code Style

**Go**: Run `gofmt -w .` and `go vet ./...` before committing. The CI runs `golangci-lint`.

**Node.js**: The project uses TypeScript with strict settings. Run `pnpm test` to check for regressions.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](./LICENSE).
