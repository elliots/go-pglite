# Toolchain image for building the PGlite WASI binary. Everything in it is
# pinned: the Debian base by digest, wasi-sdk by release and sha256.
FROM debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3

# Build tools PostgreSQL needs on the host side (perl for genbki, bison and
# flex for the parser, zic from libc-bin for the time zone data).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl xz-utils make perl bison flex python3 file \
 && rm -rf /var/lib/apt/lists/*

# wasi-sdk 34.0 (LLVM 23.1.0, wasi-libc 2e6fb9d8ee0c with libsetjmp), released 2026-08-25.
# Checksums are the ones GitHub records for the release assets.
ARG TARGETARCH
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) arch=x86_64; sha=b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4 ;; \
      arm64) arch=arm64;  sha=f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971 ;; \
      *) echo "unsupported TARGETARCH $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/wasi-sdk.tar.gz \
      "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/wasi-sdk-34.0-${arch}-linux.tar.gz"; \
    echo "${sha}  /tmp/wasi-sdk.tar.gz" | sha256sum -c -; \
    mkdir /opt/wasi-sdk; \
    tar -xzf /tmp/wasi-sdk.tar.gz -C /opt/wasi-sdk --strip-components=1; \
    rm /tmp/wasi-sdk.tar.gz

ENV WASI_SDK=/opt/wasi-sdk
ENV PATH=/opt/wasi-sdk/bin:$PATH
