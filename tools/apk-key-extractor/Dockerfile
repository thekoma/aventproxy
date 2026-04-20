FROM eclipse-temurin:21.0.10_7-jre-jammy

RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 \
    openssl \
    unzip \
    wget \
    && wget -q https://github.com/skylot/jadx/releases/download/v1.5.1/jadx-1.5.1.zip -O /tmp/jadx.zip \
    && unzip -q /tmp/jadx.zip -d /opt/jadx \
    && chmod +x /opt/jadx/bin/jadx \
    && rm /tmp/jadx.zip \
    && apt-get remove -y wget unzip && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

ENV PATH="/opt/jadx/bin:${PATH}"

WORKDIR /app
COPY extract.py .

VOLUME ["/input", "/output"]
ENTRYPOINT ["python3", "extract.py"]
CMD ["/input/app.apk"]
