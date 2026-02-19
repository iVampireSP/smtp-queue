FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS TARGETARCH

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o smtp-queue .

FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /app/smtp-queue .

ENV CONFIG_PATH=/app/config.yaml

EXPOSE 2525

ENTRYPOINT ["./smtp-queue"]
