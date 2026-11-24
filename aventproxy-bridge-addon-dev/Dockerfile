FROM golang:1.27-trixie AS builder

WORKDIR /src
COPY avent-webrtc-bridge/ .
RUN go mod download && CGO_ENABLED=0 go build -o /avent-webrtc-bridge .

# ffmpeg for the AAC copy of the audio (pkg/rtsp/aac.go), built with only
# what that needs: G.711 in from a pipe, AAC out as ADTS, static. About
# 3 MB, where Debian's ffmpeg package added 476 MB. The tarball is checked
# against the SHA-256 of the release signed by FFmpeg's release key
# (FCF986EA15E6E293A5644F10B4322F04D67658D8). Without ffmpeg the bridge
# still runs; the streams then carry G.711 only.
FROM golang:1.27-trixie AS ffmpeg
ARG FFMPEG_VERSION=7.1.2
ARG FFMPEG_SHA256=eea81393e530fdd5cf55ac12820f057d20fa681ae61864c46bb3831091d3d8e1
RUN curl -fsSLo /tmp/ffmpeg.tar.gz "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.gz" \
 && echo "${FFMPEG_SHA256}  /tmp/ffmpeg.tar.gz" | sha256sum -c - \
 && mkdir /ffmpeg && tar -xzf /tmp/ffmpeg.tar.gz -C /ffmpeg --strip-components=1 \
 && cd /ffmpeg \
 && ./configure --disable-everything --disable-autodetect --disable-network \
      --disable-doc --disable-debug --disable-ffplay --disable-ffprobe --disable-x86asm \
      --enable-small --enable-swresample \
      --enable-protocol=pipe \
      --enable-demuxer=pcm_mulaw,pcm_alaw --enable-decoder=pcm_mulaw,pcm_alaw \
      --enable-encoder=aac --enable-muxer=adts \
      --enable-filter=aresample,aformat,anull,abuffer,abuffersink \
      --extra-ldflags=-static \
 && make -j"$(nproc)" ffmpeg && strip ffmpeg

# The base sits here rather than in a build argument so that Renovate sees it.
# 13-slim follows Debian's point releases, and Renovate proposes 14 when it comes.
FROM debian:13-slim

RUN apt-get update && apt-get install -y --no-install-recommends jq ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /avent-webrtc-bridge /usr/local/bin/avent-webrtc-bridge
COPY --from=ffmpeg /ffmpeg/ffmpeg /usr/local/bin/ffmpeg
# ffmpeg is built from its 7.1.2 release with LGPL parts only (no --enable-gpl
# or --enable-nonfree); its licence travels with it.
COPY --from=ffmpeg /ffmpeg/COPYING.LGPLv2.1 /usr/share/doc/ffmpeg/COPYING.LGPLv2.1
COPY run.sh /
RUN chmod a+x /run.sh

# The bridge keeps its Tuya session cache in .tuya-data under its working
# directory. /data is the add-on's persistent volume; without this the cache
# landed in / and was lost whenever the container was recreated (issue #160).
WORKDIR /data

CMD [ "/run.sh" ]
