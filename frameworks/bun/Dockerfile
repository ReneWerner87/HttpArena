# `http2: true` for Bun.serve shipped in 1.4.1 (oven-sh/bun#40137); on 1.4.0 the
# flag is ignored rather than rejected and :8082/:8443 answer HTTP/1.1, which
# validation reports as "server responded with HTTP/1.1". `http3: true` is in
# 1.4.0 already. Override with --build-arg BUN_VERSION=canary to try a nightly.
ARG BUN_VERSION=1.4.1
FROM oven/bun:${BUN_VERSION}
WORKDIR /app
COPY server.ts entrypoint.sh ./
RUN chmod +x entrypoint.sh
EXPOSE 8080 8081 8082 8443 8443/udp
CMD ["./entrypoint.sh"]
