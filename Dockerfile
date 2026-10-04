# 多阶段构建：前端(Vite) → 后端(go:embed dist) → distroless 运行时
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
# GOPROXY 可构建时覆盖（默认官方源；国内环境 --build-arg GOPROXY=https://goproxy.cn,direct）
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /aiops ./cmd/aiops

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /aiops /aiops
EXPOSE 8090 6060
ENTRYPOINT ["/aiops", "serve", "-config", "/etc/aiops/config.yaml"]
