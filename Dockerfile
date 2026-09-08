FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
    && CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/restaurant ./cmd/restaurant

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/api /out/restaurant /usr/local/bin/
USER 10001:10001
ENTRYPOINT ["api"]
CMD ["serve"]
