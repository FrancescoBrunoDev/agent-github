# Stage 1: build the Go binary
FROM golang:1.23-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /agent ./cmd/agent

# Stage 2: minimal runtime
FROM alpine:3.21

RUN apk add --no-cache ca-certificates docker-cli

COPY --from=builder /agent /usr/local/bin/agent

EXPOSE 8080
ENTRYPOINT ["agent"]
CMD ["serve"]
