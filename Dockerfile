# Build stage
FROM golang:1.23.2-alpine3.20 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN GOOS=linux GOARCH=amd64 go build -o reminder

# Final stage
FROM alpine:3.20

WORKDIR /app

COPY --from=builder /app/reminder .

EXPOSE 8011

CMD ["./reminder"]