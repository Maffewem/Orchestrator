# Control plane image: dashboard + API + agent binaries for /install.sh.
# Multi-arch: docker buildx build --platform linux/amd64,linux/arm64 -t orchestrator .
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ENV CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o /out/orchestrator-server ./cmd/orchestrator-server
RUN set -e; for t in amd64:amd64: arm64:arm64: armv7:arm:7 armv6:arm:6; do \
      name=${t%%:*}; rest=${t#*:}; arch=${rest%%:*}; arm=${rest#*:}; \
      GOOS=linux GOARCH=$arch GOARM=$arm go build -trimpath \
        -ldflags "-s -w -X github.com/Maffewem/Orchestrator/internal/agent.Version=$VERSION" \
        -o /out/downloads/orchestrator-agent-linux-$name ./cmd/orchestrator-agent; \
    done

RUN mkdir -p /out/data

# Node.js and npm are in the final image because Cloudflare Pages and Workers projects are built here.
FROM node:22-alpine3.22
ARG VERSION=dev
LABEL org.opencontainers.image.version=$VERSION
# CA certificates come from the build stage so this image builds without network access.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/orchestrator-server /usr/local/bin/
COPY --from=build /out/downloads /downloads
COPY --from=build --chown=10001:10001 /out/data /data
USER 10001
ENV ORCH_DB=/data/orchestrator.db ORCH_DOWNLOADS=/downloads ORCH_ADDR=:8080
VOLUME /data
EXPOSE 8080
# The updater swaps containers only once the new one reports healthy.
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/api/health || exit 1
ENTRYPOINT ["orchestrator-server"]
