# Build
FROM golang:1.26.5-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/lite-cpa ./cmd/lite-cpa

# Runtime
FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata \
  && adduser -D -H -u 10001 lite
WORKDIR /app
COPY --from=build /out/lite-cpa /usr/local/bin/lite-cpa
COPY config/config.example.yaml /app/config/config.example.yaml
RUN mkdir -p /app/logs /app/data && chown -R lite:lite /app
USER lite
ENV TZ=Asia/Shanghai
EXPOSE 8317
VOLUME ["/app/logs", "/app/data"]
ENTRYPOINT ["lite-cpa"]
CMD ["--config", "/app/config/config.yaml"]
