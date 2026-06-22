# TONpayment — multi-stage build producing a tiny static image.
FROM golang:1.26-alpine AS build
# Fall through to direct VCS on any proxy error (a transitive dep can 403 on the
# Go proxy); git is needed for those direct fetches. go.sum still verifies checksums.
ENV GOPROXY=https://proxy.golang.org|direct
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.20
# ca-certificates: outbound HTTPS to toncenter + webhook endpoints.
RUN apk add --no-cache wget ca-certificates && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/server /app/server
ENV TON_HTTP_ADDR=:8080 TON_ENV=prod TON_DATA_DIR=/app/data
RUN mkdir -p /app/data && chown -R app:app /app
EXPOSE 8080
USER app
HEALTHCHECK --interval=30s --timeout=3s --start-period=20s \
  CMD wget -qO- http://localhost:8080/readyz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/server"]
