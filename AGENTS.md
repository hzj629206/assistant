# Project Overview

This is a backend-only Golang project for integrating SeaTalk with Codex CLI, Claude Code, and ACP Agents.

# Project Guidelines

- The programming language is Golang 1.25. Follow Golang's best practises and use modern language and library features.
- Always write code comments and documents in English.
- Keep the application lightweight. Do not introduce unnecessarily complex architecture or abstractions.
- Prefer the Go standard library. Avoid unnecessary third-party dependencies.
- Add logs for important actions and events to support tracing and troubleshooting.
- When importing third-party Go packages, group imports by domain name, and place this project's own packages in the last import group.
- Use `go run`, `go build`, and `go install` for build and execution workflows.
- When using `go build`, always set the output path with `-o` so the generated binary is placed under the project `bin` directory, regardless of the current working directory.
- Use `golangci-lint` **v2.11.4+** for linting and code checks.
- When executing Python scripts, always use the `python3` command instead of `python`.
- When installing Python packages or running pip commands, always use `pip3` instead of `pip`.
- When installing a global Python CLI package, use `uv tool install`.
- When only executing a Python CLI package, use `uvx`.
- When reviewing SeaTalk Open Platform docs under `https://open.seatalk.io/docs/`, use `chrome-devtools-mcp` to inspect the rendered page content instead of relying on raw HTTP fetches.

## Directory Structure

- `cmd/<name>` contains application entrypoints and process wiring.
- `config` contains process-wide configuration parsing and defaults.
- `adapter` contains bridge code between concrete platform integrations and the agent layer, such as translating SeaTalk callbacks into normalized agent messages and binding agent replies back to SeaTalk operations.
- `agent` contains the chat-agent layer shared across platform integrations, including normalized inbound message models, conversation state, asynchronous dispatching, responder abstractions, and agent runner integrations.
- `internal` contains non-exported application packages and shared implementation details that should not be imported by external modules.
- `seatalk` contains the SeaTalk platform integration, including callback handling, event models, and OpenAPI client code.
- `cache` contains lightweight storage abstractions and implementations used by the application.

## Dependencies
- Codex SDK (Golang): `github.com/godeps/codex-sdk-go` v0.1.x. Similar to official SDK `@openai/codex-sdk` (TypeScript).
- AppServer SDK (Golang): `github.com/pmenglund/codex-sdk-go`.
