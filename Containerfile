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

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/ai-usage-web /ai-usage-web
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/ai-usage-web"]
