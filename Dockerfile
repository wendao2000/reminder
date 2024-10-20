# Build stage
FROM golang:1.23.2 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY .env ./
RUN GOOS=linux GOARCH=amd64 go build -o reminder

CMD ["./reminder"]