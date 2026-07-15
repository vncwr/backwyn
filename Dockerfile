# build stage.
FROM golang:1.26-alpine AS build
WORKDIR /src

# download dependencies.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# disable cgo for static binary.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backwyn ./cmd/backwyn

FROM alpine:3.21

# install postgres client tools and ca-certificates.
RUN apk add --no-cache postgresql17-client ca-certificates

COPY --from=build /out/backwyn /usr/local/bin/backwyn

# run as non-root user.
RUN adduser -D -u 10001 backwyn
USER backwyn

# use /tmp for backup staging.
ENV TMPDIR=/tmp

ENTRYPOINT ["backwyn"]
CMD ["run", "-interval", "6h", "-max-age", "24h"]
