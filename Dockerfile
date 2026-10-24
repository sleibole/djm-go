FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /djm .

FROM litestream/litestream:0.5.17 AS litestream

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
 && adduser -D -H -u 65532 djm \
 && mkdir /data && chown djm:djm /data
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream
COPY --from=build /djm /djm
COPY litestream.yml /etc/litestream.yml
COPY entrypoint.sh /entrypoint.sh
USER djm
ENV ENV=prod DB_PATH=/data/djm.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
