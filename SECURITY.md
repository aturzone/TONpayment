# Security

## Design: non-custodial and watch-only

TONpayment **never holds private keys, never signs, and never moves funds.** It
only *watches* a public TON address and confirms that a payment matching an
invoice has landed on-chain. This is the single most important property of the
project, and it is why the repository is safe to be public. Please help keep it
that way — do not add key handling, signing, or any fund-movement code.

## What actually protects a payment

The receiving address is public by nature, and the on-chain comment (memo) is
public and unauthenticated. A payment is only accepted when **all three** of the
following match an invoice:

1. the **receiving address** (the address whose transactions we query),
2. the exact **memo** (a random per-invoice comment), and
3. an **amount ≥** the invoice amount (overpayment is accepted).

That triple — address + memo + amount — is the real protection. The memo alone,
or the amount alone, is not enough.

The memo is 128 bits of randomness, and the store **enforces** that `(receiving
address, memo)` is unique (a unique index in Postgres; an in-memory index
otherwise). So memo uniqueness is a hard guarantee, not a probability: a single
on-chain payment can never be matched to two different invoices. Receiving
addresses are also validated and canonicalized (CRC-checked) before use, so a
mistyped address is rejected up front rather than silently matching nothing.

## Fail-closed verification

The verifier **fails closed**: any error talking to toncenter, any malformed
response, any non-matching transaction results in the invoice staying `pending`.
An invoice is only ever marked `paid` on a positive, matching confirmation.
Settlement is **claim-once**: an atomic store transition guarantees an invoice
settles (and fires its webhook) exactly once, even under concurrent checks.

## Finality

A transaction returned by toncenter `getTransactions(archival)` is already in a
committed masterchain block. TON has deterministic finality, so there is no
probabilistic "0-conf" reorg window to wait out.

## Verification scope (known limitation)

The verifier reads the receiving address's most recent **~30 incoming
transactions** from toncenter and matches by memo + amount. On a high-traffic
*shared* receiving address, a genuine payment could be pushed out of that window
before it is observed. Because each memo is unique per receiving address (enforced
by the store), this can only ever cause a **missed** match (the invoice stays
`pending`), never a wrong or duplicate credit. Mitigate by polling frequently
and/or using a dedicated receiving address per integration.

## Payment semantics & caveats

- A payment is matched by the **(receiving address, memo, amount)** triple,
  fail-closed, and settled exactly once. `paid` means a matching incoming message
  carrying the memo with `value ≥ amount` landed on the receiving address.
- Use a receiving address that **accepts and keeps** a plain transfer with a text
  comment — a normal wallet, or a non-bouncing contract. A bounceable contract
  that rejects the transfer can show an incoming message yet not net-receive the
  funds; prefer non-bounceable (`UQ`) receiving addresses.
- **Split / partial payments are not summed** — a single transfer must meet the
  amount. Overpayment settles; underpayment stays pending.
- The address must match the configured network (`TON_NETWORK`, default
  `mainnet`); a wrong-network `payTo` is rejected at create time.
- `GET /v1/invoices/{id}/status` runs an on-demand verify. It is idempotent and
  reflects on-chain truth (it cannot fabricate a payment), and is gated by the API
  key when one is configured — so it is safe against cross-site / crawler triggers.

## Webhooks

Webhook delivery is **best-effort** (retried with backoff) and can be lost if the
process crashes mid-delivery. Always verify the `X-Signature` HMAC, and treat the
invoice status from the API as authoritative before doing anything irreversible.

## Data exposure

An invoice's `metadata` is echoed on every read (`GET /v1/invoices/{id}` and
`/status`) and in webhooks. All read endpoints are gated by the API key when one
is configured (the invoice ID is an unguessable token, **not** an authorization),
but you should still **never store secrets in metadata**.

## Secrets

- **No secrets are committed.** `.env` is gitignored; only `.env.example` ships.
- The receiving address, toncenter API key, optional create-API key, and webhook
  secret are all read from the environment.
- The optional create-API key is compared in constant time.
- Webhook bodies are signed with `X-Signature: sha256=<hex HMAC-SHA256(secret, body)>`
  so receivers can verify authenticity. **Always verify the signature** before
  trusting a webhook, and treat the invoice as informational — re-check status
  via the API if a payment drives anything irreversible.

## Reporting a vulnerability

Please open a private report via GitHub Security Advisories (preferred) or email
the maintainer. Do not file public issues for security problems. Include steps to
reproduce and the impact you observed; you'll get an acknowledgement as soon as
possible.
