# `http2: true` for Bun.serve landed after 1.4.0, so the released 1.4 image
# would bind :8443 and :8082 and quietly answer HTTP/1.1 on both — the flag is
# ignored rather than rejected there, which validation catches as "server
# responded with HTTP/1.1". Pin back to oven/bun:1.4 once 1.4.1 ships; the
# server code needs no change.
ARG BUN_IMAGE=oven/bun:canary
FROM ${BUN_IMAGE}
WORKDIR /app
COPY server.ts entrypoint.sh ./
RUN chmod +x entrypoint.sh
EXPOSE 8082 8443
CMD ["./entrypoint.sh"]
