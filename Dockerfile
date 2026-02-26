FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY . .
RUN CGO_ENABLED=0 go build -mod=vendor -ldflags="-s -w" -o m4d-coso .

FROM alpine:latest
RUN apk add --no-cache tzdata ca-certificates
WORKDIR /app
COPY --from=builder /app/m4d-coso .
VOLUME ["/app/sessions"]
CMD ["./m4d-coso"]
