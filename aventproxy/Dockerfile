ARG BUILD_FROM=python:3.14-alpine
FROM ${BUILD_FROM}

ARG BUILD_ARCH
ARG BUILD_VERSION

LABEL \
    io.hass.name="Avent RTSP Proxy" \
    io.hass.description="RTSP proxy for Philips Avent / Tuya cameras" \
    io.hass.arch="${BUILD_ARCH}" \
    io.hass.type="addon" \
    io.hass.version="${BUILD_VERSION}"

COPY proxy /app/proxy
COPY run.sh /

RUN chmod a+x /run.sh

WORKDIR /app
EXPOSE 8554

CMD ["/run.sh"]
