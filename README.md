# CLI Proxy API

CLIProxyAPI is a proxy server that provides Claude-compatible API interfaces for CLI tooling.

You can access Claude models locally and with multiple accounts through any Claude-compatible or OpenAI-compatible (Chat Completions and Responses) client or SDK.

## Overview

- Claude-compatible API endpoints (`/v1/messages`) for Claude models
- OpenAI-compatible Chat Completions and Responses endpoints translated to Claude
- Claude Desktop subscription support via Desktop OAuth enrollment
- Claude API key support (`claude-api-key`) with custom Anthropic-compatible base URLs
- Streaming and non-streaming responses
- Function calling/tools support
- Multimodal input support (text and images)
- Multiple accounts with round-robin and session-affinity load balancing
- Claude Desktop multi-account load balancing
- Reusable Go SDK for embedding the proxy (see `docs/sdk-usage.md`)

### Claude Desktop alignment branch

On `feature/claude-desktop-alignment`, the `claude` provider is reserved for independently enrolled Claude Desktop OAuth accounts. API keys and custom Anthropic-compatible gateways use `anthropic-compatible` instead. Requests are rendered from the captured Desktop profile, and Renderer, embedded SDK, Segment, Datadog logs, Datadog RUM, and Sentry delivery paths are isolated per enrolled account.

The embedded v140609 bundle uses schema 8 and pins the captured event-state transition artifact. Enrollment discovers auxiliary telemetry material from the locally installed official Desktop package and stores it only in the protected credential envelope. If material is missing, management telemetry reports `awaiting-enrollment-material`; model traffic remains available and the management UI exposes the degraded endpoint instead of silently hiding it.

On non-Windows hosts, Claude Desktop magic-link attestation launches a local Chromium or Chrome process with an isolated temporary profile. Install `chromium`, `chromium-browser`, `google-chrome-stable`, or `google-chrome`, or set `CLIPROXY_CLAUDE_DESKTOP_CHROMIUM_PATH` to the executable. Headless mode is enabled by default; set `CLIPROXY_CLAUDE_DESKTOP_CHROMIUM_HEADLESS=false` only when an interactive display is available. The browser uses the configured `proxy-url` through a loopback-only CONNECT bridge so proxy credentials are not placed in the browser command line. If Chromium cannot initialize its own sandbox inside an already hardened service environment such as systemd with `NoNewPrivileges=true`, set `CLIPROXY_CLAUDE_DESKTOP_CHROMIUM_NO_SANDBOX=true`; use this compatibility option only when the outer service sandbox is trusted and retained.

## Getting Started

```bash
go run ./cmd/server --config config.yaml
```

See `config.example.yaml` for the full configuration reference.

## Management API

see [MANAGEMENT_API.md](https://help.router-for.me/management/api)

## SDK Docs

- Usage: [docs/sdk-usage.md](docs/sdk-usage.md)
- Advanced (executors & translators): [docs/sdk-advanced.md](docs/sdk-advanced.md)
- Access: [docs/sdk-access.md](docs/sdk-access.md)
- Watcher: [docs/sdk-watcher.md](docs/sdk-watcher.md)

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
