FROM golang:alpine AS builder
WORKDIR /usr/src
RUN apk update && apk add --no-cache git
# pre-copy/cache go.mod for pre-downloading dependencies and only redownloading them in subsequent builds if they change
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY .. .
RUN go build -o app ./cmd/main.go

FROM alpine:latest
COPY --from=builder /usr/src/app /app
WORKDIR /env
ENTRYPOINT ["/app"]