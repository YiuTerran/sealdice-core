FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS ui-build
WORKDIR /src/sealdice-ui
RUN corepack enable && corepack prepare pnpm@10.10.0 --activate
COPY sealdice-ui/package.json sealdice-ui/pnpm-lock.yaml sealdice-ui/pnpm-workspace.yaml ./
COPY sealdice-ui/ ./
RUN pnpm install --frozen-lockfile && pnpm run build-only

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS go-build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN rm -rf static/frontend && mkdir -p static/frontend
COPY --from=ui-build /src/sealdice-ui/dist/ /src/static/frontend/
RUN go generate ./signature
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/sealdice-core .

FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
WORKDIR /app
RUN sed -i 's|http://deb.debian.org/debian-security|http://mirrors.aliyun.com/debian-security|g; s|http://deb.debian.org/debian|http://mirrors.aliyun.com/debian|g' /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app/data /app/backups /app/cache \
    && chown -R 10001:10001 /app
COPY --from=go-build --chown=10001:10001 /out/sealdice-core /app/sealdice-core
USER 10001:10001
VOLUME ["/app/data"]
EXPOSE 18081
ENTRYPOINT ["/app/sealdice-core"]
CMD ["--container-mode", "--hide-ui", "--address=127.0.0.1:18311"]
