# Build both binaries from the same module, then ship them in a minimal static
# image. docker-compose runs the same image twice with different entrypoints
# (prober and udmexporter).
FROM golang:1.25 AS build
WORKDIR /src

# Cache dependencies separately from source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Stamp the prober with the git commit (and build time) it was built from, so
# the running binary — and the dashboard's "Running build" panel — report
# exactly what code is deployed. The whole repo (including .git) is in the build
# context, so we read the SHA here; a "-dirty" suffix flags an unclean tree, and
# it falls back to "unknown" when built outside a git checkout. CGO is disabled
# so the result is a fully static binary that runs in distroless.
RUN COMMIT="$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)" \
 && { git diff --quiet 2>/dev/null || COMMIT="${COMMIT}-dirty"; } \
 && BUILT="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.commit=${COMMIT} -X main.buildTime=${BUILT}" -o /out/prober ./cmd/prober \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/udmexporter ./cmd/udmexporter

FROM gcr.io/distroless/static-debian12:latest
COPY --from=build /out/prober /usr/local/bin/prober
COPY --from=build /out/udmexporter /usr/local/bin/udmexporter
# Default config; override by bind-mounting over this path.
COPY config/targets.yml /etc/justrebootit/targets.yml

# 9430 = prober metrics, 9431 = udm exporter metrics.
EXPOSE 9430 9431

# Default to the prober; the udm-exporter service overrides this in compose.
ENTRYPOINT ["/usr/local/bin/prober"]
CMD ["-config", "/etc/justrebootit/targets.yml"]
