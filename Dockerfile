# syntax=docker/dockerfile:1
#
# 多阶段构建：编译面板，并交叉编译常用平台的 agent 二进制一起打包，
# 这样容器内的面板就能通过 /downloads/agent/<os>/<arch> 把自己分发给待接入的机器。

ARG GO_VERSION=1.24

# ---- 构建阶段 -------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# 仅标准库依赖，先复制 go.mod 以复用缓存层。
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=docker
# 面板二进制
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/opback-panel ./cmd/panel
# 各平台 agent 二进制（命名需与面板的下载处理器一致）
RUN set -eux; \
    mkdir -p /out/agents; \
    for target in linux/amd64 linux/arm64 linux/arm windows/amd64 darwin/arm64; do \
      os="${target%%/*}"; arch="${target##*/}"; ext=""; \
      if [ "$os" = "windows" ]; then ext=".exe"; fi; \
      GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
        -o "/out/agents/opback-agent-${os}-${arch}${ext}" ./cmd/agent; \
    done

# ---- 运行阶段 -------------------------------------------------------------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 -h /app panel

WORKDIR /app
COPY --from=build /out/opback-panel /app/bin/opback-panel
COPY --from=build /out/agents /app/bin/
COPY deploy/config.example.json /app/config.example.json

RUN mkdir -p /data && chown -R panel:panel /data /app
USER panel

ENV OPBACK_DATA=/data \
    OPBACK_AGENT_DIR=/app/bin \
    OPBACK_WEB_ADDR=:8080 \
    OPBACK_AGENT_ADDR=:8081

EXPOSE 8080 8081
VOLUME ["/data"]

ENTRYPOINT ["/app/bin/opback-panel"]
