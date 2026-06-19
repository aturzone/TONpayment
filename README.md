# TONpayment

[![CI](https://github.com/aturzone/TONpayment/actions/workflows/ci.yml/badge.svg)](https://github.com/aturzone/TONpayment/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
![Go](https://img.shields.io/github/go-mod/go-version/aturzone/TONpayment)

A small, **non-custodial, watch-only** TON payment service in Go. It issues
invoices, watches the TON blockchain, and confirms payment — without ever
holding keys or moving funds.

- **Create an invoice** → get a unique memo, an amount (in nanoTON), a receiving
  address, a `ton://transfer` deeplink, and a QR code.
- **The payer pays** from any TON wallet (TON Connect, deeplink, or manual
  transfer with the memo as the comment).
- **The service confirms** by reading the receiving address's incoming
  transactions from the [toncenter](https://toncenter.com) v2 API and matching
  by **memo + amount**. On payment it records the tx hash and (optionally) fires
  a **signed webhook**.

It is a thin, auditable verification layer you can run yourself and point any
app at — the donor of its payment logic is a production e-commerce backend, and
that logic is reused here verbatim where it matters (the memo+amount match, the
fail-closed verifier, the claim-once settlement).

> **Module path:** this repo's module is `github.com/aturzone/TONpayment`
> (matching the GitHub repo). If you fork it under a different name, change the
> first line of [`go.mod`](go.mod) and run `go mod tidy`.

## Why non-custodial matters

TONpayment **never has private keys, never signs, and never moves money.** The
receiving address is public by nature; the only thing it does is *observe* the
chain. That's why this repo is safe to be public — and why you should keep it
that way (see [SECURITY.md](SECURITY.md)). The real protection for a payment is
the **(receiving address + memo + amount)** triple, checked **fail-closed**: any
error leaves the invoice `pending`, never `paid`.

## 60-second quickstart

Requires Go 1.26+.

```sh
git clone git@github.com:aturzone/TONpayment.git
cd TONpayment

# A receiving address is required to create invoices. In dev (TON_ENV=dev) the
# verifier is a MOCK that auto-confirms after 2 status checks — no real funds.
export TON_RECEIVING_ADDRESS="UQ...your_address..."
export TON_ENV=dev

go run ./cmd/server      # listens on :8080
```

In another terminal:

```sh
# 1) create an invoice for 2.5 TON (2_500_000_000 nanoTON), valid 15 min
curl -s -X POST localhost:8080/v1/invoices \
  -H 'content-type: application/json' \
  -d '{"amountNano":2500000000,"ttlSeconds":900,"metadata":{"orderId":"abc-123"}}'

# -> { "id":"inv_...", "status":"pending", "memo":"TON-xxxxxxxx",
#      "deeplink":"ton://transfer/UQ...?amount=2500000000&text=TON-xxxxxxxx", ... }

# 2) check status (the mock confirms on the 2nd call)
curl -s localhost:8080/v1/invoices/<id>/status   # status: pending
curl -s localhost:8080/v1/invoices/<id>/status   # status: paid ✅
```

In production set `TON_ENV=prod` with a real `TON_RECEIVING_ADDRESS`; the service
then uses the real toncenter verifier.

## How a payer pays

You hand the payer the invoice's `deeplink` (and/or the QR at
`/v1/invoices/{id}/qr`). Three equivalent ways to pay:

1. **Deeplink** — open `ton://transfer/<address>?amount=<nano>&text=<memo>` in a
   wallet (tap on mobile, or render the QR for desktop wallets to scan).
2. **QR** — show the PNG from `GET /v1/invoices/{id}/qr`; the payer scans it.
3. **Manual** — send the exact amount to the address, putting the **memo in the
   transaction comment**. (The memo is what links the payment to the invoice — it
   must be exact.)

For a full **TON Connect** integration, host a manifest like
[`tonconnect-manifest.json`](tonconnect-manifest.json) and use
[@tonconnect/ui](https://github.com/ton-connect/sdk) on your frontend to build
the same transfer (same `address`, `amount`, and `text`/payload as the deeplink).
Either way, your backend just polls `GET /v1/invoices/{id}/status` (or receives
the webhook) and reacts when `status` becomes `paid`.

## API

Base path `/v1`. All responses are JSON except the QR (PNG). CORS origins are
configurable; errors are `{ "error": "message" }`.

### `POST /v1/invoices` — create an invoice

If `TON_CREATE_API_KEY` is set, send it as `Authorization: Bearer <key>` or
`X-API-Key: <key>`.

Request body:

| field        | type              | required | notes                                            |
|--------------|-------------------|----------|--------------------------------------------------|
| `amountNano` | integer           | yes      | amount in **nanoTON** (1 TON = 1e9). Must be > 0. |
| `ttlSeconds` | integer           | no       | invoice lifetime; defaults to `TON_DEFAULT_TTL_SECONDS`. |
| `metadata`   | object (str→str)  | no       | your own reference (orderId, userId, …); opaque to the service. **Returned on every read — don't put secrets here.** Capped at 64 keys / 8 KB. |

Returns `201` with the invoice:

```json
{
  "id": "inv_Hr0bx95etpRW",
  "status": "pending",
  "payTo": "UQ...",
  "memo": "TON-ca5808f8",
  "amountNano": 2500000000,
  "amount": "2.5",
  "currency": "TON",
  "txHash": "",
  "metadata": { "orderId": "abc-123" },
  "deeplink": "ton://transfer/UQ...?amount=2500000000&text=TON-ca5808f8",
  "qr": "/v1/invoices/inv_Hr0bx95etpRW/qr",
  "createdAt": "2026-06-19T11:22:09Z",
  "paidAt": "0001-01-01T00:00:00Z",
  "expiresAt": "2026-06-19T11:37:09Z"
}
```

### `GET /v1/invoices/{id}` — fetch an invoice

Returns the current invoice (no chain lookup).

### `GET /v1/invoices/{id}/status` — verify + settle, then return

Triggers an **on-demand** verification: if a matching payment is on-chain the
invoice flips to `paid` (and the webhook fires); if it's unpaid and past its TTL
it flips to `expired`; otherwise it stays `pending`. Idempotent and safe to call
concurrently. You don't *have* to call this — the background poller does the same
sweep on a timer — but it's there for instant feedback.

### `GET /v1/invoices/{id}/qr` — QR PNG

Returns an `image/png` QR code of the invoice's deeplink. Optional `?size=<px>`
(64–1024, default 256).

### `GET /healthz` — liveness

`{ "ok": true, "service": "tonpayment", "env": "..." }`.

## Configuration

All configuration is via `TON_*` environment variables (see
[`.env.example`](.env.example)). No secrets are read from or written to disk.

| Variable                    | Default                          | Description                                                        |
|-----------------------------|----------------------------------|--------------------------------------------------------------------|
| `TON_HTTP_ADDR`             | `:8080`                          | Listen address. `PORT` is also honored (for PaaS).                 |
| `TON_ENV`                   | `dev`                            | `dev` (mock verifier) or `prod` (toncenter verifier; requires address). |
| `TON_ALLOWED_ORIGINS`       | `http://localhost:5173,…:4173`   | Comma-separated CORS allow-list.                                   |
| `TON_TRUST_PROXY`           | `false`                          | Trust `X-Forwarded-For` for rate-limit keying (only behind a proxy). |
| `TON_RECEIVING_ADDRESS`     | *(empty)*                        | **Required to create invoices.** The watch-only receiving address. |
| `TON_API_BASE`              | `https://toncenter.com/api/v2`   | toncenter v2 API base URL.                                         |
| `TON_API_KEY`               | *(empty)*                        | Optional toncenter key (raises rate limit above ~1 req/s).         |
| `TON_DATABASE_URL`          | *(empty)*                        | If set, use Postgres; otherwise in-memory/JSON.                    |
| `TON_DATA_DIR`              | `data`                           | Directory for the JSON store (when not using Postgres).            |
| `TON_DEFAULT_TTL_SECONDS`   | `900`                            | Default invoice lifetime when a request omits `ttlSeconds`.        |
| `TON_CREATE_API_KEY`        | *(empty)*                        | If set, `POST /v1/invoices` requires this key.                     |
| `TON_POLL_ENABLED`          | `true`                           | Run the background settle/expire poller.                           |
| `TON_POLL_INTERVAL_SECONDS` | `10`                             | Poller interval.                                                   |
| `TON_POLL_CONCURRENCY`      | `4`                              | Max concurrent verifications per poll tick.                        |
| `TON_WEBHOOK_URL`           | *(empty)*                        | If set, POST the invoice JSON here on payment.                     |
| `TON_WEBHOOK_SECRET`        | *(empty)*                        | HMAC-SHA256 secret for the `X-Signature` header.                   |

## Webhooks

If `TON_WEBHOOK_URL` is set, the service POSTs the full invoice JSON to it when an
invoice settles, with header:

```
X-Signature: sha256=<hex HMAC-SHA256(TON_WEBHOOK_SECRET, body)>
```

Verify the signature before trusting the payload. Delivery is asynchronous and
retried a few times with exponential backoff. Example verification (Node):

```js
import crypto from "node:crypto";
function verify(rawBody, header, secret) {
  const expected = "sha256=" + crypto.createHmac("sha256", secret).update(rawBody).digest("hex");
  return crypto.timingSafeEqual(Buffer.from(header), Buffer.from(expected));
}
```

## Storage

- **In-memory / JSON** (default): state is kept in memory and mirrored to
  `TON_DATA_DIR/store.json`. Great for dev and small single-instance deploys.
- **Postgres** (set `TON_DATABASE_URL`): durable, and the right choice for
  production. The schema (one `invoices` table, indexed on `status`) is created
  automatically on boot.

Both implement the same `Store` interface; settlement uses an atomic
claim-once transition (`UPDATE … WHERE status='pending'`) so an invoice is paid
exactly once even across concurrent checks.

## Run with Docker

```sh
# in-memory store
docker compose up --build

# with Postgres
docker compose --profile postgres up --build
#   then uncomment TON_DATABASE_URL in docker-compose.yml
```

The image defaults to `TON_ENV=prod`, so you **must** provide
`TON_RECEIVING_ADDRESS` (e.g. `export TON_RECEIVING_ADDRESS=UQ...` before
`docker compose up`, or set it in your environment) — otherwise the container
exits with a clear error. The image is a multi-stage static build on Alpine with a
`/healthz` healthcheck.

## Deploy notes

- Set `TON_ENV=prod` and a real `TON_RECEIVING_ADDRESS`.
- Set `TON_API_KEY` to avoid toncenter's anonymous rate limit.
- Use Postgres (`TON_DATABASE_URL`) for anything beyond a single ephemeral
  instance. The in-process per-invoice lock makes settlement atomic *within* one
  instance; the atomic store claim keeps it correct *across* instances too.
- Put it behind TLS (a reverse proxy). Lock down `TON_ALLOWED_ORIGINS` and
  consider setting `TON_CREATE_API_KEY` so only your backend can mint invoices.
- The webhook is best-effort; treat `GET /status` as the source of truth before
  doing anything irreversible.

## Limitations & operational notes

- **Verification scope.** The verifier checks the **most recent ~30 incoming
  transactions** of the receiving address (toncenter `getTransactions`). On a busy
  *shared* address, a payment could scroll out of that window before it's seen.
  Mitigation: keep the poll interval short, and/or use a **dedicated receiving
  address** per integration. The memo is unique per invoice, so funds are never
  misattributed — the risk is a missed (not a wrong) match.
- **Webhooks are best-effort.** They are retried with backoff but can be lost if
  the process crashes mid-delivery. Treat `GET /v1/invoices/{id}/status` (or the
  poller-updated state) as the source of truth before doing anything irreversible,
  and always verify the `X-Signature`.
- **Mock verifier is dev-only.** With `TON_ENV=prod` the service uses the real
  toncenter verifier and refuses to start without `TON_RECEIVING_ADDRESS`; it never
  falls back to the auto-confirming mock in prod.
- **Single-instance JSON store.** The in-memory/JSON store rewrites the whole file
  per change and keeps terminal invoices; use Postgres for production scale.

## Development

```sh
make run     # go run ./cmd/server
make test    # go test ./...
make vet     # go vet ./...
make build   # -> bin/server
make docker  # build the image
```

The verifier's matching logic and the settlement idempotency are covered by
tests (`internal/wallet`, `internal/service`, `internal/httpx`). The Postgres
integration test runs only when `TON_TEST_DATABASE_URL` points at a disposable
database; otherwise it's skipped.

## Layout

```
cmd/server          entrypoint: wiring, verifier selection, poller, shutdown
internal/money      nanoTON integer math
internal/idgen      short random IDs
internal/store      Invoice type + Store interface; in-memory/JSON + Postgres
internal/wallet     Verifier interface; toncenter verifier; mock; NewMemo
internal/service    invoice lifecycle: create, verify, claim-once settle, expire
internal/deeplink   ton://transfer builder + QR PNG
internal/webhook    signed, retrying webhook sender
internal/poller     background settle/expire sweep
internal/httpx      router, middleware, handlers
```

## License

[MIT](LICENSE).
