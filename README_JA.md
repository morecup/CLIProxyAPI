# CLI Proxy API

[English](README.md) | [中文](README_CN.md) | 日本語

CLIProxyAPI は CLI ツール向けに Claude 互換 API インターフェースを提供するプロキシサーバーです。

Claude 互換または OpenAI 互換（Chat Completions / Responses）のクライアントや SDK から、ローカルまたは複数アカウントで Claude モデルにアクセスできます。

## 概要

- Claude 互換 API エンドポイント（`/v1/messages`）
- OpenAI 互換の Chat Completions / Responses エンドポイント（Claude プロトコルへ変換）
- Desktop OAuth 登録による Claude Desktop サブスクリプション対応
- Claude API key 対応（`claude-api-key`）、カスタム Anthropic 互換 base URL も可能
- ストリーミング / 非ストリーミングレスポンス
- Function calling / tools 対応
- マルチモーダル入力（テキストと画像）
- 複数アカウントのラウンドロビンおよびセッションアフィニティ負荷分散
- Claude Desktop のマルチアカウント負荷分散
- プロキシを埋め込める Go SDK（`docs/sdk-usage.md` 参照）

## はじめに

```bash
go run ./cmd/server --config config.yaml
```

設定項目の詳細は `config.example.yaml` を参照してください。

## Management API

[MANAGEMENT_API.md](https://help.router-for.me/management/api) を参照

## SDK ドキュメント

- 使い方：[docs/sdk-usage.md](docs/sdk-usage.md)
- 応用（executor / translator）：[docs/sdk-advanced.md](docs/sdk-advanced.md)
- アクセス制御：[docs/sdk-access.md](docs/sdk-access.md)
- Watcher：[docs/sdk-watcher.md](docs/sdk-watcher.md)

## コントリビューション

Pull Request を歓迎します。

## ライセンス

MIT License — 詳細は [LICENSE](LICENSE) を参照。
