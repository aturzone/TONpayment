# Contributing to TONpayment

Thanks for your interest! TONpayment is a small, deliberately auditable
non-custodial TON payment service. Contributions that keep it simple, correct,
and safe are very welcome.

## The one hard rule: stay non-custodial

This service is **watch-only**. It must **never** hold private keys, sign
transactions, or move funds — that is the entire reason it is safe to run and to
keep public. Pull requests that add key handling, signing, custody, or any
fund-movement path will not be accepted. See [SECURITY.md](SECURITY.md).

## Development setup

Requires **Go 1.26+**.

```sh
git clone git@github.com:aturzone/TONpayment.git
cd TONpayment
make test     # go test ./...
make run      # run the server in dev (set TON_RECEIVING_ADDRESS first)
```

Before opening a PR, make sure all of these pass — CI runs the same:

```sh
gofmt -l .        # must print nothing
go vet ./...
go build ./...
go test -race ./...
```

If you change the verifier, the store, or the settlement path, please run the
race detector (`go test -race ./...`) — the concurrency guarantees are the core
of this project. The Postgres integration test runs only when
`TON_TEST_DATABASE_URL` points at a disposable database.

## Guidelines

- **Keep the payment-matching logic exact and fail-closed.** Any error must leave
  an invoice `pending`, never `paid`. Add tests for new behavior.
- **No secrets in the repo.** Configuration is environment-only; only
  `.env.example` ships.
- **Match the surrounding style.** Standard `gofmt`, small focused packages,
  comments that explain *why*.
- Keep dependencies minimal and well-justified.

## Submitting changes

1. Open an issue first for anything non-trivial, so we can agree on the approach.
2. Branch from `main`, keep commits focused, write a clear PR description.
3. Ensure the checks above pass and tests cover the change.

By contributing you agree your contributions are licensed under the project's
[MIT License](LICENSE).
