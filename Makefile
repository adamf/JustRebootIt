# Convenience targets. The day-to-day workflow is just `make up`.
.PHONY: up down logs build test vet fmt tidy

# Bring up the whole stack (builds images on first run). Grafana: http://localhost:3000
up:
	docker compose up -d --build

# Tear it down (keeps the Prometheus/Grafana volumes).
down:
	docker compose down

# Follow logs from all services.
logs:
	docker compose logs -f

# Build both Go binaries locally into ./bin.
build:
	CGO_ENABLED=0 go build -o bin/prober ./cmd/prober
	CGO_ENABLED=0 go build -o bin/udmexporter ./cmd/udmexporter

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy
