ARG BUILD_FROM
FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY avent-webrtc-bridge/ .
RUN go mod download && CGO_ENABLED=0 go build -o /avent-webrtc-bridge .

FROM ${BUILD_FROM}

RUN apt-get update && apt-get install -y --no-install-recommends jq ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /avent-webrtc-bridge /usr/local/bin/avent-webrtc-bridge
COPY run.sh /
RUN chmod a+x /run.sh

CMD [ "/run.sh" ]
