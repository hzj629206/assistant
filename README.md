# Assistant

Backend-only Golang project for integrating SeaTalk with local agent CLIs such as Codex CLI and Claude Code.

## How It Works

The service receives SeaTalk bot callbacks from both direct chats and group chats, normalizes inbound messages, and dispatches them to the configured agent runner.

The SeaTalk integration supports common bot-facing message forms, including text messages, image messages, file messages, video messages, combined-forwarded messages, quoted messages, and interactive cards.

The agent layer manages conversation context per chat thread so the bot can reply inside the same thread and continue the conversation across follow-up messages in that thread.
When the bot is mentioned from the middle of an existing group chat thread, the service loads the full thread history so the bot can respond with the surrounding context.
Technically, when a new agent thread is created for an existing SeaTalk thread, the service first loads that chat thread's history into the initial turn context.
Messages from the middle of a group chat thread that do not mention the bot are still dispatched to the agent, and the agent is guided to treat them as optional-reply messages.

If another message arrives for the same chat thread while an earlier batch is already queued or running,
the dispatcher appends it to that thread's pending batch and processes it as the next merged turn after the current one finishes.

For non-text inbound messages, the dispatcher keeps a short merge window before starting the turn.
If a follow-up text message arrives in the same chat thread during that window, both messages are processed together as one turn;
otherwise the non-text message is processed after the timeout.
This applies to private top-level chats, private threads, and group threads. Specifically for private top-level chats, the service treats SeaTalk top-level messages as one shared chat thread for conversation continuity,
and anchors replies for a merged batch to the last merged message.

Inbound interactive cards are preserved as interactive-card messages after normalization.
The service extracts a compact text summary from card titles, descriptions, action buttons, redirect links, and images,
while still keeping interactive-card semantics distinct from plain user text.

Interactive card callback buttons use a JSON callback action payload serialized into the SeaTalk button `value`.
The supported actions are `tool_call`, which asks the agent to execute a selected tool call,
and `prompt`, which submits a new user prompt into the current conversation thread when the button is clicked.

For outbound bot replies, interactive cards are required when the user explicitly asks for progress reporting,
or when it needs to present images or links instead of relying only on plain text.

In real-time conversations, edited messages and updated interactive-card messages do not take effect for the bot.
SeaTalk message update events are not visible to bots, so the service only processes the original message content delivered in the callback.

At runtime, the Go service communicates with the locally installed agent CLI selected by the daemon you run, such as `codex` for `codexd` or `claude` for `clauded`.
This keeps the application lightweight while allowing the local agent environment to provide skills, MCP servers, and other agent capabilities.

When the service shuts down, the dispatcher stops accepting new work immediately.
It drops messages that are still waiting in the in-memory queue, pending batch, or delayed merge window,
it also drops slash commands that are still queued but have not started yet,
and only keeps already-running turns or commands alive for a short grace period.

## Key Features

- SeaTalk bot callback handling and reply delivery for both direct chats and group chats
- Thread-aware conversation handling so the bot can reply and continue chatting in the same thread
- Delayed merge for non-text inbound messages so follow-up explanatory text in the same chat thread can be processed together
- Per-thread queued merge handling so messages that arrive during an active or queued turn are appended to the next pending batch
- Support for text messages, image messages, file messages, video messages, combined-forwarded messages, quoted messages, and interactive cards
- Interactive-card summaries that preserve card semantics, including button labels, redirect links, and card images
- Interactive-card callback actions for both tool execution and button-triggered prompt submission
- Conversation control slash commands, including `/stop` to interrupt the current turn and `/reset` to interrupt the current turn and reset the persisted conversation state
- Agent support for sending generated data files such as CSV, JSON, and text reports back to users
- Outbound interactive-card guidance for complex task progress updates and structured presentation of complex results
- Shared agent layer for normalized message processing and conversation state management
- Runner integrations for local agent CLIs, including Codex CLI and Claude Code
- Lightweight cache abstractions for storing state
- Flexible configuration through defaults, YAML config files, and command-line flags
- Deployment-friendly design for public callback endpoints, including reverse SSH forwarding support

## Requirements

This project requires `Go 1.25+`.

This project depends on a locally installed agent CLI for the daemon you choose to run.

The Go service communicates with the local CLI executable at runtime. This repository does not bundle or manage those CLIs itself.

For `codexd`, install and configure the local `Codex CLI`.
For `clauded`, install and configure the local `Claude Code` CLI.
For `acpd`, install and configure a local `ACP`-compatible agent CLI.

If you want to extend the local agent capabilities used by this project, configure the selected CLI installation directly.
For example, in environments that support those features, you can:

- add or enable `MCP` servers in the local CLI configuration
- install or manage local skills in the CLI environment

Those extensions are picked up through the local CLI used by the service.

Codex discovers and loads applicable `AGENTS.md` files from the directory context where it is started.
Claude Code supports `CLAUDE.md` in the same role for workspace-specific instructions.
You can use those files to inject project-specific context, coding conventions, and workflow instructions when `codexd` launches Codex or `clauded` launches Claude Code inside the relevant workspace.

If MCP tool outputs are truncated in the local `Codex CLI`, increase `tool_output_token_limit` in the `Codex CLI` configuration.
This Codex-specific setting is managed by the local `Codex CLI`, not by this repository.

## Onboarding

Before running the service in production or for SeaTalk callback testing, complete the following setup:

1. Create an application on the SeaTalk Open Platform and enable the Bot capability. See the SeaTalk guide: <https://open.seatalk.io/docs/quickly-build-a-bot>.
   In the app permission page, manually enable `Get Thread by Thread ID in Group Chat`. This permission is not selected by default.
   After you save the permission change, it takes effect automatically and does not require additional approval.
   The bot can attach employee information to private-thread context when that capability is enabled.
   That capability requires additional platform approval and is disabled by default in this service through the `seatalk.employee_info_enabled` configuration toggle.
2. Set up `Nginx` on a machine with public internet access, and configure the domain, HTTPS, and reverse proxy for this service.
3. Run this project with SeaTalk Bot `app_id`/`app_secret`/`signing_secret` obtained in step 1. If the service runs on a local machine without public inbound access, expose the callback endpoint through one of the supported traffic-entry approaches:
   - Use the built-in reverse SSH forwarding support so a public `Nginx` host can reach the callback endpoint through `tunnel.ssh_addr`, `tunnel.ssh_user`, and `tunnel.ssh_key`.
   - Or use the built-in `Cloudflare Tunnel` integration through `tunnel.cloudflared_token` to publish the local service through a public HTTPS endpoint managed by Cloudflare.

   When reverse SSH forwarding is enabled with `tunnel.ssh_addr`, the service reuses only the port from `listen_addr` and requests a remote listen address in the form `:<port>`.

   For OpenSSH, remote forwarding behavior is still restricted by `sshd_config`:
   - `AllowTcpForwarding yes` must allow remote forwarding.
   - `GatewayPorts clientspecified` is recommended if you want the server to honor the requested remote bind semantics.
   - With the default `GatewayPorts no`, the server may force the remote listener onto loopback even when the client requests `:<port>`.

   When `tunnel.cloudflared_token` is configured and `tunnel.ssh_addr` is empty, the service starts `cloudflared tunnel run --token <token>` and forwards traffic to the local `listen_addr`.
   You must configure the Cloudflare Tunnel route separately so the public hostname points to this local HTTP service port.
4. Configure the SeaTalk callback URL as `https://<domain>/callback` in the SeaTalk platform. If you use `Cloudflare Tunnel`, use the public HTTPS URL provided by that tunnel. If you use reverse SSH forwarding, use the public domain served by your `Nginx` host.

Security note: Restrict the bot's access scope to the minimum necessary to reduce the risk of misuse, abuse, and attacks by others.

## Configuration

The process loads configuration in this order:

1. built-in defaults
2. the default config file at `$HOME/.assistant/config.yml`, if it exists
3. a config file passed with `-config` or `-f`
4. command-line flags

Later sources override earlier ones.

See [config.yml.example](./config.yml.example) for a complete example with all supported fields and inline descriptions.

Use `<daemon> -h` to see the supported flags.

## Install

### Install `codexd`

Install the `codexd` daemon binary into your Go bin directory:

```bash
go install github.com/hzj629206/assistant/cmd/codexd@master
```

After installation, run the service with `codexd`.

Security note for `codexd`: the service runs with `read-only` sandbox mode and `never` approval by default, while `WebSearch` and `NetworkAccess` are enabled by default for the local `codex` CLI backend.
Do not store sensitive data in the working directory because the bot is able to read files from that directory, search the web, and access network resources.

### Install `clauded`

Install the `clauded` daemon binary into your Go bin directory:

```bash
go install github.com/hzj629206/assistant/cmd/clauded@master
```

After installation, run the service with `clauded`.

Security note for `clauded`: the service inherits the permissions and tool-access behavior of the local `Claude Code` CLI environment. The default permission mode is `dontAsk`, and any permission request that still requires user confirmation is currently accepted by default.
Do not store sensitive data in the working directory because the bot may be able to read local files, invoke configured tools, and access external resources depending on that local CLI configuration.

### Install `acpd`

Install the `acpd` daemon binary into your Go bin directory:

```bash
go install github.com/hzj629206/assistant/cmd/acpd@master
```

After installation, run the service with `acpd`.

Security note for `acpd`: the service launches a locally configured `ACP`-compatible agent process and inherits the capabilities, permissions, and authentication flow exposed by that local agent runtime. Any permission request that still requires user confirmation is currently accepted silently by default.
Do not store sensitive data in the working directory because the configured `ACP` agent may be able to read local files, invoke tools, and access external resources depending on its implementation and configuration.
