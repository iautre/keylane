FROM node:22-alpine AS web-build
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27.4-alpine AS go-build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-build /web/dist ./internal/webui/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/keylane ./cmd/keylane

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=go-build /out/keylane /usr/local/bin/keylane
RUN mkdir -p /data && chown -R app:app /data
USER app
ENV PORT=8080 DATABASE_PATH=/data/keylane.db
ENV DATABASE_ENABLED=false
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/keylane"]
