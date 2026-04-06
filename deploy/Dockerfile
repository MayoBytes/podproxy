# Build stage
FROM golang:1.26-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /podproxy .

# Final stage — small Alpine image includes ca-certificates and tzdata
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /podproxy .
VOLUME ["/app/data", "/app/cache"]
EXPOSE 8080
ENTRYPOINT ["/app/podproxy"]
