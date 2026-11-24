FROM golang:1.27-trixie AS builder

WORKDIR /src
COPY avent-webrtc-bridge/ .
RUN go mod download && CGO_ENABLED=0 go build -o /avent-webrtc-bridge .

# The base sits here rather than in a build argument so that Renovate and image
# scanners see it. Distroless static has no shell and no packages, only CA
# certificates, tzdata and a root user, which is all the static bridge needs
# (issue #124). It has no version tag to follow: a rebuild picks up Debian 13's
# updates, and the move to static-debian14 is an edit by hand. The root variant,
# not :nonroot, on purpose: the bridge writes /data and reads /config.
FROM gcr.io/distroless/static-debian13

COPY --from=builder /avent-webrtc-bridge /usr/local/bin/avent-webrtc-bridge

# The bridge keeps its Tuya session cache in .tuya-data under its working
# directory. /data is the add-on's persistent volume; without this the cache
# landed in / and was lost whenever the container was recreated (issue #160).
WORKDIR /data

# supervise took over from run.sh: it finds the integration's config, runs the
# bridge on it and restarts the bridge when it exits or the config changes.
ENTRYPOINT ["/usr/local/bin/avent-webrtc-bridge", "supervise"]
