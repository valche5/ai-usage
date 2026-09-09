# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ai-usage-web ./cmd/ai-usage-web \
    && mkdir -p /out/data \
    && touch /out/data/.keep \
    && chmod 0700 /out/data

# Rootless Podman maps container uid 0 to the host user. A USER 65532
# instruction would run as a subuid and break the /data volume.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/data /data
COPY --from=build /out/ai-usage-web /ai-usage-web
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/ai-usage-web"]
