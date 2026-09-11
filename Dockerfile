FROM node:20-bookworm-slim AS frontend
WORKDIR /app/frontend

COPY frontend/package.json frontend/yarn.lock* ./
RUN yarn install --frozen-lockfile

COPY frontend/ ./
RUN yarn build

FROM golang:1.25-bookworm AS builder
WORKDIR /app

ENV CGO_ENABLED=1

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=frontend /app/frontend/dist ./cmd/go-mumble-server/frontend-dist

RUN go build -ldflags "-s -w" -tags embed_frontend -o go-mumble-server ./cmd/go-mumble-server

FROM debian:bookworm-slim
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl gosu openssl \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd -r -g 999 mumble && useradd -r -u 999 -g 999 -d /data -s /usr/sbin/nologin mumble

COPY --from=builder /app/go-mumble-server ./
COPY configs/mumble-server.toml ./mumble-server.toml
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

RUN mkdir -p /data && chown mumble:mumble /data

ENV MUMBLE_DATABASE_PATH=/data/mumble-server.sqlite
ENV MUMBLE_NETWORK_HOST=0.0.0.0

EXPOSE 64738/tcp
EXPOSE 64738/udp
EXPOSE 64730/tcp

VOLUME ["/data"]

ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["-config", "mumble-server.toml"]
