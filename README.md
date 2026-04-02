# Assistant

Backend-only Golang project for integrating SeaTalk with Codex.

## How It Works

The service receives SeaTalk bot callbacks from both direct chats and group chats,
normalizes inbound messages into a shared agent model, and dispatches them to the Codex-backed assistant layer.

The SeaTalk integration supports common bot-facing message forms, including text messages, image messages, file messages, video messages, combined-forwarded messages, quoted messages, and interactive cards.

The agent layer manages conversation context per chat thread so the assistant can reply inside the same thread and continue the conversation across follow-up messages in that thread.
When the bot is mentioned from the middle of an existing group chat thread, the service loads the full thread history so the assistant can respond with the surrounding context.
Technically, when a new agent thread is created for an existing SeaTalk thread, the service first loads that chat thread's history into the initial turn context.
Messages from the middle of a group chat thread that do not mention the bot are still dispatched to the agent, and the agent is guided to treat them as optional-reply messages.

If another message arrives for the same chat thread while an earlier batch is already queued or running,
the dispatcher appends it to that thread's pending batch and processes it as the next merged turn after the current one finishes.

For non-text inbound messages, the dispatcher keeps a short merge window before starting the turn.
If a follow-up text message arrives in the same chat thread during that window, both messages are processed together as one turn;
otherwise the non-text message is processed after the timeout.
This applies to private top-level chats, private threads, and group threads. Specifically for private top-level chats, the service treats SeaTalk top-level messages as one shared chat thread for conversation continuity,
and anchors replies for a merged batch to the last merged message.

Inbound interactive cards are preserved as interactive-card messages in the shared agent model.
The service extracts a compact text summary from card titles, descriptions, buttons, redirect links, and images,
while still keeping interactive-card semantics distinct from plain user text.

Interactive card callback buttons use a JSON callback action payload serialized into the SeaTalk button `value`.
The supported actions are `tool_call`, which asks the assistant to execute a selected tool call,
and `prompt`, which submits a new user prompt into the current conversation thread when the button is clicked.

For outbound assistant replies, interactive cards are required when the user explicitly asks for progress reporting,
or when it needs to present images or links instead of relying only on plain text.

In real-time conversations, edited messages and updated interactive-card messages do not take effect for the bot.
SeaTalk message update events are not visible to bots, so the service only processes the original message content delivered in the callback.

At runtime, the Go service communicates with the locally installed `codex` CLI.
This keeps the application lightweight while allowing the local Codex environment to provide skills, MCP servers, and other assistant capabilities.

When the service shuts down, the dispatcher stops accepting new work immediately.
It drops messages that are still waiting in the in-memory queue, pending batch, or delayed merge window,
and only keeps already-running turns alive for a short grace period.

## Key Features

- SeaTalk bot callback handling and reply delivery for both direct chats and group chats
- Thread-aware conversation handling so the assistant can reply and continue chatting in the same thread
- Delayed merge for non-text inbound messages so follow-up explanatory text in the same chat thread can be processed together
- Per-thread queued merge handling so messages that arrive during an active or queued turn are appended to the next pending batch
- Support for text messages, image messages, file messages, video messages, combined-forwarded messages, quoted messages, and interactive cards
- Interactive-card summaries that preserve card semantics, including button labels, redirect links, and card images
- Interactive-card callback actions for both assistant tool execution and button-triggered prompt submission
- Agent support for sending generated data files such as CSV, JSON, and text reports back to users
- Outbound interactive-card guidance for complex task progress updates and structured presentation of complex results
- Shared agent layer for normalized message processing and conversation state management
- Codex runner integration through the local `codex` CLI
- Lightweight cache abstractions for storing assistant-related state
- Flexible configuration through defaults, YAML config files, and command-line flags
- Deployment-friendly design for public callback endpoints, including reverse SSH forwarding support

## Requirements

This project requires `Go 1.25+`.

This project depends on a locally installed `Codex CLI`.

The Go service communicates with the local `codex` executable at runtime. This repository does not bundle or manage the CLI itself.

If you want to extend Codex capabilities for this project, configure the local `Codex CLI` installation directly, for example:

- add or enable `MCP` servers in the `Codex CLI` configuration
- install or manage `Skills` in the `Codex CLI` environment

Those extensions are picked up through the local `Codex CLI` used by the service.

If MCP tool outputs are truncated in the local `Codex CLI`, increase `tool_output_token_limit` in the `Codex CLI` configuration.
This setting is managed by the local `Codex CLI`, not by this repository.

## Onboarding

Before running the service in production or for SeaTalk callback testing, complete the following setup:

1. Create an application on the SeaTalk Open Platform and enable the Bot capability. See the SeaTalk guide: <https://open.seatalk.io/docs/quickly-build-a-bot>.
   In the app permission page, manually enable `Get Thread by Thread ID in Group Chat`. This permission is not selected by default.
   After you save the permission change, it takes effect automatically and does not require additional approval.
   The bot can attach employee information to private-thread context when that capability is enabled.
   That capability requires additional platform approval and is disabled by default in this service through the `seatalk.employee_info_enabled` configuration toggle.
2. Set up `Nginx` on a machine with public internet access, and configure the domain, HTTPS, and reverse proxy for this service.
3. Run this project. If the service runs on a local machine without public inbound access, expose the callback endpoint through one of the supported traffic-entry approaches:
   - Use the built-in reverse SSH forwarding support so the public `Nginx` host can reach the callback endpoint.
   - Or use a third-party tunnel service such as `ngrok` or `Cloudflare Tunnel` to publish the local service through a public HTTPS endpoint.

   When reverse SSH forwarding is enabled with `remote_ssh_addr`, the service reuses only the port from `listen_addr` and requests a remote listen address in the form `:<port>`.

   For OpenSSH, remote forwarding behavior is still restricted by `sshd_config`:
   - `AllowTcpForwarding yes` must allow remote forwarding.
   - `GatewayPorts clientspecified` is recommended if you want the server to honor the requested remote bind semantics.
   - With the default `GatewayPorts no`, the server may force the remote listener onto loopback even when the client requests `:<port>`.
4. Configure the SeaTalk callback URL as `https://<domain>/callback` in the SeaTalk platform. If you use `ngrok` or `Cloudflare Tunnel`, use the public HTTPS URL provided by that service.

## Configuration

The process loads configuration in this order:

1. built-in defaults
2. the default config file at `$HOME/.assistant/config.yml`, if it exists
3. a config file passed with `-config` or `-f`
4. command-line flags

Later sources override earlier ones.

## Install

Install the daemon binary into your Go bin directory:

```bash
go install github.com/hzj629206/assistant/cmd/codexd@master
```

After installation, run the service with `codexd`.

Security note: the service runs with `read-only` sandbox mode and `never` approval by default, while `WebSearch` and `NetworkAccess` are enabled by default for the local `codex` CLI backend.
Do not store sensitive data in the working directory because the bot is able to read files from that directory, search the web, and access network resources.
Grant the bot only the minimum permissions it needs so you reduce the blast radius of abuse, prompt misuse, and malicious attacks.

### 1. Use The Default Config File

The service primarily relies on a YAML config file. If `$HOME/.assistant/config.yml` exists, it is loaded automatically.

Example `$HOME/.assistant/config.yml`:

```yaml
listen_addr: :8421
remote_ssh_addr: example.com
remote_ssh_user: ubuntu
ssh_key_path: /path/to/id_rsa
codex:
  backend: appserver
  model: gpt-5.4-mini
  reasoning_effort: low
  sandbox: read-only
seatalk:
  app_id: your-app-id
  app_secret: your-app-secret
  signing_secret: your-signing-secret
  employee_info_enabled: false
```

Supported `codex.sandbox` values are `read-only`, `workspace-write`, and `danger-full-access`.

When a config file is loaded, the service logs the resolved file path.

Run:

```bash
codexd
```

### 2. Override A Few Common Options From CLI

Command-line flags are only intended for a small set of common non-sensitive overrides:

```bash
codexd \
  -listen-addr :8421 \
  -codex-backend appserver \
  -codex-model gpt-5.4-mini \
  -codex-reasoning-effort low \
  -codex-sandbox read-only
```

Common flags: `codexd -h`

### 3. Use A Custom Config File

Pass a YAML config file with `-config` or `-f` to override the default config path.

Example:

```bash
codexd -config /path/to/config.yml
```
