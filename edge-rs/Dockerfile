ARG RUST_IMAGE=rust:1.90-alpine
ARG ALPINE_IMAGE=alpine:3.21

FROM ${RUST_IMAGE} AS builder

RUN apk add --no-cache build-base pkgconfig perl

WORKDIR /app/edge-rs

COPY Cargo.toml Cargo.lock ./
COPY src ./src

RUN cargo build --release --locked

FROM ${ALPINE_IMAGE}

RUN apk add --no-cache ca-certificates tzdata wget && rm -rf /var/cache/apk/*

RUN addgroup -g 1000 sub2api && \
    adduser -u 1000 -G sub2api -s /bin/sh -D sub2api

WORKDIR /app

COPY --from=builder --chown=sub2api:sub2api /app/edge-rs/target/release/sub2api-edge-rs /app/sub2api-edge-rs

USER sub2api
EXPOSE 18080
CMD ["/app/sub2api-edge-rs"]
