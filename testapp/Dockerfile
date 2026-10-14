# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o server main.go

# Final runtime stage
FROM alpine:3.20

WORKDIR /app
COPY --from=builder /app/server /app/server

EXPOSE 8080
ENV PORT=8080

CMD ["/app/server"]
