# CLI Proxy API

[English](README.md) | 中文 | [日本語](README_JA.md)

CLIProxyAPI 是一个为 CLI 工具提供 Claude 兼容 API 接口的代理服务器。

您可以通过任何 Claude 兼容或 OpenAI 兼容（Chat Completions 与 Responses）的客户端或 SDK，以本地方式或多账户访问 Claude 模型。

## 概览

- Claude 兼容 API 端点（`/v1/messages`）
- OpenAI 兼容的 Chat Completions 与 Responses 端点（翻译为 Claude 协议）
- 通过 Desktop OAuth 注册支持 Claude Desktop 订阅
- Claude API key 支持（`claude-api-key`），支持自定义 Anthropic 兼容 base URL
- 流式与非流式响应
- Function calling / tools 支持
- 多模态输入（文本与图像）
- 多账户轮询与会话亲和负载均衡
- Claude Desktop 多账户负载均衡
- 可复用的 Go SDK（见 `docs/sdk-usage_CN.md`）

## 快速开始

```bash
go run ./cmd/server --config config.yaml
```

完整配置项说明见 `config.example.yaml`。

## 管理 API

见 [MANAGEMENT_API.md](https://help.router-for.me/management/api)

## SDK 文档

- 用法：[docs/sdk-usage_CN.md](docs/sdk-usage_CN.md)
- 进阶（executor 与 translator）：[docs/sdk-advanced_CN.md](docs/sdk-advanced_CN.md)
- 访问控制：[docs/sdk-access_CN.md](docs/sdk-access_CN.md)
- Watcher：[docs/sdk-watcher_CN.md](docs/sdk-watcher_CN.md)

## 贡献

欢迎提交 Pull Request。

## 许可证

本项目基于 MIT License 发布，详见 [LICENSE](LICENSE)。
