# Project Overview

This is a backend-only Golang project for integrating SeaTalk with Codex.

# Project Guidelines

- The programming language is Golang 1.25. Follow Golang's best practises and use modern language and library features.
- Always write code comments and documents in English.
- Keep the application lightweight. Do not introduce unnecessarily complex architecture or abstractions.
- Prefer the Go standard library. Avoid unnecessary third-party dependencies.
- Add logs for important actions and events to support tracing and troubleshooting.
- When importing third-party Go packages, group imports by domain name, and place this project's own packages in the last import group.
- When using foundational variable names like `err` and `ok`, avoid shadowing them within the same logical context.
- Use `go run`, `go build`, and `go install` for build and execution workflows.
- When using `go build`, always set the output path with `-o` so the generated binary is placed under the project `bin` directory, regardless of the current working directory.
- Use `golangci-lint` v2 for linting and code checks.
- When executing Python scripts, always use the `python3` command instead of `python`.
- When installing Python packages or running pip commands, always use `pip3` instead of `pip`.
- When installing a global Python CLI package, use `uv tool install`.
- When only executing a Python CLI package, use `uvx`.
- When reviewing SeaTalk Open Platform docs under `https://open.seatalk.io/docs/`, use `chrome-devtools-mcp` to inspect the rendered page content instead of relying on raw HTTP fetches.
- When working in JetBrains IDEs, write jump locations in responses as `[file.go:line](/absolute/path/to/file.go)`, don't include line number in the actual path.

## Directory Structure

- `cmd/<name>` contains application entrypoints and process wiring.
- `config` contains process-wide configuration parsing and defaults.
- `adapter` contains bridge code between concrete platform integrations and the agent layer, such as translating SeaTalk callbacks into normalized agent messages and binding agent replies back to SeaTalk operations.
- `agent` contains the chat-agent layer shared across platform integrations, including normalized inbound message models, conversation state, asynchronous dispatching, responder abstractions, and Codex runner integrations.
- `seatalk` contains the SeaTalk platform integration, including callback handling, event models, and OpenAPI client code.
- `cache` contains lightweight storage abstractions and implementations used by the application.

## Dependencies
- Codex SDK (Golang): `github.com/godeps/codex-sdk-go`. Similar to official SDK `@openai/codex-sdk` (TypeScript).
- AppServer SDK (Golang): `github.com/pmenglund/codex-sdk-go`.
