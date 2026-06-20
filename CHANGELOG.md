# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). The first
tagged release will be `v0.1.0`.

## [Unreleased]

### Added
- Machine-readable OpenAPI spec at `api/openapi.yaml` — the contract clients (web,
  mobile) generate typed SDKs from.
- Per-request receiving address: `POST /v1/invoices` accepts an optional `payTo`
  (validated + canonicalized), enabling multi-tenant use where each caller is paid
  to its own address. `TON_RECEIVING_ADDRESS` becomes an optional default. Prod
  refuses to start with neither a default nor `TON_CREATE_API_KEY` (so it is never
  an open, arbitrary-address invoice minter).
- Resource bounds: caps on invoice TTL (`TON_MAX_TTL_SECONDS`, default 24h) and on
  the number of pending invoices in total (`TON_MAX_PENDING`) and per receiving
  address (`TON_MAX_PENDING_PER_ADDRESS`), so an unbounded pending set can't exhaust
  the toncenter budget. The default max TTL drops from 7 days to 24h.
- `internal/tonaddr`: TON address parser/validator/normalizer (raw and
  user-friendly forms, CRC-16/XMODEM checksum). The receiving address is now
  validated and canonicalized at startup — prod fails fast on a malformed
  address instead of silently leaving every invoice stuck pending.
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
- Memo entropy raised to 128 bits and uniqueness made a hard guarantee: the store
  enforces a unique `(pay_to, memo)` (a unique index in Postgres, an in-memory
  index otherwise), so a single on-chain payment can never settle two invoices.
- Production never falls back to the auto-confirming mock verifier, and refuses to
  start as an open, arbitrary-address minter (it needs a default address or a
  create-API-key gate).
- Verifier defense-in-depth: besides memo + amount, it now also checks the
  transaction destination (by account identity, across address representations) and
  ignores transactions that predate the invoice.
- Postgres queries are bounded by a 10s timeout, and shutdown bounds the webhook
  drain so a slow callback can't hang termination.
- `X-Forwarded-For` is only trusted when `TON_TRUST_PROXY` is set.
- Fail-closed verification; constant-time create-API-key comparison; no secrets in
  the repository.
