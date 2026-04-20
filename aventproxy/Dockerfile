ARG BUILD_FROM
FROM ${BUILD_FROM:-python:3.12-alpine}

COPY proxy /app/proxy
COPY run.sh /

RUN chmod a+x /run.sh

WORKDIR /app
EXPOSE 8554

CMD ["/run.sh"]
