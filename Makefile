.PHONY: build run test probe devup devdown pf-otel tidy fmt vet

build:
	go build -o bin/aiops ./cmd/aiops

run:
	go run ./cmd/aiops serve

probe:
	go run ./cmd/aiops probe

# dev 模式把集群内 Jaeger 的 OTLP gRPC 转发到本地 4317（trace 上报需要，另开终端保持运行）
pf-otel:
	kubectl port-forward svc/makejob-jaeger 4317:4317

devup:
	docker compose -f deploy/docker-compose.dev.yml up -d

devdown:
	docker compose -f deploy/docker-compose.dev.yml down

test:
	go test ./...

tidy:
	go mod tidy

fmt:
	gofmt -l -w .

vet:
	go vet ./...
