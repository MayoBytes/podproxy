# Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /podproxy .

# Final stage — small Alpine image includes ca-certificates and tzdata
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /podproxy .
VOLUME ["/app/data", "/app/cache"]
EXPOSE 8080
ENTRYPOINT ["/app/podproxy"]
