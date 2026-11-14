# ---- 构建阶段 ----
FROM golang:1.23-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux \
    GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/beian2api .

# ---- 运行阶段 ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 app && \
    mkdir -p /data && chown app:app /data
WORKDIR /app
COPY --from=build /out/beian2api /usr/local/bin/beian2api
USER app
ENV BEIAN2API_DATA_DIR=/data \
    BEIAN2API_LISTEN=:8080
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["beian2api"]
CMD ["serve"]
