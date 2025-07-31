FROM golang:1.24-alpine AS builder
WORKDIR /app
ENV GOPROXY=https://goproxy.cn,direct

COPY go.mod go.sum ./
RUN go mod tidy
COPY . .
RUN go build -o webhook main.go

FROM alpine:3.19
WORKDIR /root/
COPY --from=builder /app/webhook .
# COPY ./bin/tag-validation-webhook /root/webhook
COPY certs /certs
CMD ["./webhook", "--tls-cert-file=/certs/cert", "--tls-key-file=/certs/key"]

