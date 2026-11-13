# syntax=docker/dockerfile:1

# ---- build: static Go binary, pure-Go SQLite (no CGO) ----
FROM golang:1-alpine AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/helpdesk . && \
    mkdir -p /out/data

# ---- run: ~2 MB base, no shell, non-root ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/helpdesk /helpdesk
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV HELPDESK_DATA_DIR=/data \
    HELPDESK_ADDR=:8080
EXPOSE 8080
VOLUME /data
USER nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/helpdesk", "healthcheck"]
ENTRYPOINT ["/helpdesk"]
