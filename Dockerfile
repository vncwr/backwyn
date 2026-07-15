# Build backwyn and ship it with the PostgreSQL client binaries it shells out to.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Download modules as their own layer
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off: a static binary runs on any alpine/distroless base without libc pain.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backwyn ./cmd/backwyn

FROM alpine:3.21

# postgresql17-client: pg_dump, pg_restore, psql — the tools internal/pgtools
#   shells out to. Without these every command fails at the Require() check.
# ca-certificates: TLS to S3/R2 and to the source database (sslmode=verify-full).
RUN apk add --no-cache postgresql17-client ca-certificates

COPY --from=build /out/backwyn /usr/local/bin/backwyn

# Run unprivileged. The engine only needs to write temp files and, on the local
# backend, its storage dir — never anything system-owned.
RUN adduser -D -u 10001 backwyn
USER backwyn

# Backups are staged through temp files before being encrypted and uploaded
ENV TMPDIR=/tmp

ENTRYPOINT ["backwyn"]
CMD ["run", "-interval", "6h", "-max-age", "24h"]
