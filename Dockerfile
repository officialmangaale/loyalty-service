FROM golang:1.24-alpine AS builder

RUN apk add --no-cache ca-certificates
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/loyalty-service . && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/loyalty-migrate ./cmd/migrate && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/loyalty-reconcile ./cmd/reconcile

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/loyalty-service /loyalty-service
COPY --from=builder /out/loyalty-migrate /loyalty-migrate
COPY --from=builder /out/loyalty-reconcile /loyalty-reconcile

USER 65532:65532
EXPOSE 8086

ENTRYPOINT ["/loyalty-service"]
