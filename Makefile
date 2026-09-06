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

# ---- C6 部署（构建镜像 → 载入 kind → Helm 安装到集群）----
docker-build:
	docker build -t aiops-server:dev .

kind-load:
	kind load docker-image aiops-server:dev --name makejob

deploy: docker-build kind-load
	helm upgrade --install aiops deploy/helm/aiops -n default \
	  --set image.tag=dev \
	  --set secrets.pgPassword=$${PG_PASSWORD:-aiops_dev} \
	  --set secrets.llmApiKey="$${LLM_API_KEY}" \
	  --set secrets.jwtSecret="$${JWT_SECRET:-dev-secret}"

undeploy:
	helm uninstall aiops -n default
