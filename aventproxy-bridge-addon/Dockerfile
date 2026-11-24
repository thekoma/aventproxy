FROM golang:1.27-trixie AS builder

WORKDIR /src
COPY avent-webrtc-bridge/ .
RUN go mod download && CGO_ENABLED=0 go build -o /avent-webrtc-bridge .

# The base sits here rather than in a build argument so that Renovate sees it.
# 13-slim follows Debian's point releases, and Renovate proposes 14 when it comes.
FROM debian:13-slim

RUN apt-get update && apt-get install -y --no-install-recommends jq ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /avent-webrtc-bridge /usr/local/bin/avent-webrtc-bridge
COPY run.sh /
RUN chmod a+x /run.sh

# The bridge keeps its Tuya session cache in .tuya-data under its working
# directory. /data is the add-on's persistent volume; without this the cache
# landed in / and was lost whenever the container was recreated (issue #160).
WORKDIR /data

CMD [ "/run.sh" ]
