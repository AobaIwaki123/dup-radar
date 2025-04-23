FROM golang:1.24 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o dup-radar ./cmd/dup-radar

FROM gcr.io/distroless/base

COPY --from=builder /app/dup-radar /usr/local/bin/dup-radar

CMD ["dup-radar"]
