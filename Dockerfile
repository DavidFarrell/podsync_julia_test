FROM golang:1.25 AS builder

ENV TAG="nightly"
ENV COMMIT=""

WORKDIR /build

COPY . .

RUN make build

# Download youtube-dl - use build arg to bust cache and always get latest
ARG CACHEBUST=1
RUN wget -O /usr/bin/yt-dlp https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp && \
    chmod +x /usr/bin/yt-dlp

# Bumped 3.21 -> 3.24 (2026-06-18): 3.21 ships deno 2.0.6 which current yt-dlp flags
# "(unsupported)", breaking the YouTube n-challenge (EJS) solver so only image formats
# are returned. An interim bump to 3.22 (deno 2.3.1) fixed it but sat on yt-dlp's bare
# minimum deno floor, leaving no headroom against future yt-dlp releases. 3.24 ships
# deno 2.7.4 (plus newer ffmpeg), matching upstream mxpv/podsync's fix and giving real
# headroom. deno stays apk-managed and musl-native, matched to the same Alpine release
# as python3/ffmpeg. Builder is golang:1.25 to match upstream.
FROM alpine:3.24

WORKDIR /app

RUN apk --no-cache add ca-certificates python3 py3-pip ffmpeg tzdata \
    # https://github.com/golang/go/issues/59305
    libc6-compat \
    # Deno JS runtime required for yt-dlp YouTube challenge solving (EJS)
    # See: https://github.com/yt-dlp/yt-dlp/wiki/EJS
    deno && \
    ln -s /lib/libc.so.6 /usr/lib/libresolv.so.2 && \
    # Install Python dependencies for better YouTube support
    pip3 install --no-cache-dir --break-system-packages pycryptodomex websockets brotli

COPY --from=builder /usr/bin/yt-dlp /usr/bin/youtube-dl
COPY --from=builder /usr/bin/yt-dlp /usr/bin/yt-dlp
COPY --from=builder /build/bin/podsync /app/podsync

# Create startup script that updates yt-dlp before running podsync
RUN echo '#!/bin/sh' > /app/startup.sh && \
    echo 'echo "Checking for yt-dlp updates..."' >> /app/startup.sh && \
    echo 'wget -q -O /tmp/yt-dlp-new https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp && chmod +x /tmp/yt-dlp-new && mv /tmp/yt-dlp-new /usr/bin/yt-dlp && echo "yt-dlp updated to $(/usr/bin/yt-dlp --version)"' >> /app/startup.sh && \
    echo 'cp /usr/bin/yt-dlp /usr/bin/youtube-dl' >> /app/startup.sh && \
    echo 'exec /app/podsync "$@"' >> /app/startup.sh && \
    chmod +x /app/startup.sh

ENTRYPOINT ["/app/startup.sh"]
CMD ["--no-banner"]
