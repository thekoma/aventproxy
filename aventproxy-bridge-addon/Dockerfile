ARG BUILD_FROM
FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY tuya-ipc-terminal/ .
RUN go mod download && CGO_ENABLED=0 go build -o /tuya-ipc-terminal .

FROM ${BUILD_FROM}

RUN apt-get update && apt-get install -y --no-install-recommends jq && rm -rf /var/lib/apt/lists/*

COPY --from=builder /tuya-ipc-terminal /usr/local/bin/tuya-ipc-terminal
COPY run.sh /
RUN chmod a+x /run.sh

CMD [ "/run.sh" ]
