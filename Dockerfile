# syntax=docker/dockerfile:1

# yt-dlp and deno are pinned deliberately:
#   - yt-dlp breaks regularly when YouTube changes extraction. A pinned version means a break
#     is fixed by bumping one ARG and rebuilding, not by chasing a moving target (ADR 0001).
#   - deno is a HARD dependency, not optional. Without a JS runtime yt-dlp silently loses
#     format coverage, which would show up as "extraction failed" with no obvious cause.
FROM golang:1.23-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/yt-digest ./cmd/yt-digest

FROM debian:bookworm-slim

ARG YTDLP_VERSION=2026.08.19
ARG DENO_VERSION=2.9.7

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ffmpeg python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

# yt-dlp from a pinned release asset, so the image does not drift with PyPI.
RUN curl -fsSL -o /usr/local/bin/yt-dlp \
      "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp_linux" \
    && chmod +x /usr/local/bin/yt-dlp \
    && /usr/local/bin/yt-dlp --version

RUN curl -fsSL "https://deno.land/install.sh" | DENO_INSTALL=/usr/local sh -s -- -y \
    && rm -rf /usr/local/lib/deno /root/.deno

COPY --from=build /out/yt-digest /usr/local/bin/yt-digest

# Non-root: the bot writes only into the mounted vault, and nothing else.
RUN useradd --create-home --uid 1000 ytdigest
USER ytdigest
WORKDIR /home/ytdigest

ENTRYPOINT ["/usr/local/bin/yt-digest"]
