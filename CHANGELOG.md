# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). The first
tagged release will be `v0.1.0`.

## [Unreleased]

### Added
- Non-custodial, watch-only TON payment service.
- Invoice lifecycle: create (memo + amount + receiving address), on-chain
  verification via toncenter v2 (match by memo + amount, fail-closed), and
  exactly-once (claim-once) settlement.
- HTTP API: `POST /v1/invoices`, `GET /v1/invoices/{id}`,
  `GET /v1/invoices/{id}/status`, `GET /v1/invoices/{id}/qr`, `GET /healthz`.
- `ton://transfer` deeplink builder and QR-code PNG endpoint.
- Stores: in-memory/JSON and Postgres (pgx) behind one interface.
- Background poller (bounded concurrency) to settle/expire pending invoices.
- Optional HMAC-SHA256-signed webhooks with retry/backoff and bounded concurrency.
- Configurable CORS; optional API key gate on invoice creation; security headers;
  per-IP rate limiting with eviction.
- Docker (multi-stage) + docker-compose (optional Postgres profile); CI with
  `-race` and a Postgres integration test.

### Security
- Production refuses to start without `TON_RECEIVING_ADDRESS` and never falls back
  to the auto-confirming mock verifier.
- `X-Forwarded-For` is only trusted when `TON_TRUST_PROXY` is set.
- Fail-closed verification; constant-time create-API-key comparison; no secrets in
  the repository.
