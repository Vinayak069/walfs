FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/walfs ./cmd/walfs

FROM alpine:3.21
RUN apk add --no-cache fuse3 curl
COPY --from=build /out/walfs /usr/local/bin/walfs
COPY deploy/entrypoint.sh /entrypoint.sh
ENV WALFS_IMAGE=/data/walfs.img WALFS_SIZE=2G WALFS_MOUNT=/mnt/walfs WALFS_JOURNAL=ordered WALFS_METRICS=:9100
EXPOSE 9100
VOLUME /data
ENTRYPOINT ["/entrypoint.sh"]
