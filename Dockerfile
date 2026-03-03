# Stage 1 — base: enable pnpm via corepack
FROM node:22-alpine AS base
RUN corepack enable && corepack prepare pnpm@latest --activate
WORKDIR /app

# Stage 2 — fetcher: download all packages from lockfile into pnpm store.
# This layer invalidates ONLY when pnpm-lock.yaml changes, not on source changes.
# Result: fast rebuilds when only TypeScript files change.
FROM base AS fetcher
COPY pnpm-lock.yaml ./
RUN pnpm fetch

# Stage 3 — builder: install from pnpm store (offline), copy source, compile.
FROM fetcher AS builder
COPY package.json pnpm-lock.yaml ./
# --offline: read from the store fetched above; no network required
RUN pnpm install --frozen-lockfile --offline
COPY . .
RUN pnpm build

# Stage 4 — production: minimal runtime image.
# Only production deps and compiled dist/. No TypeScript, no tsx, no tsup.
FROM node:22-alpine AS production
RUN corepack enable && corepack prepare pnpm@latest --activate
WORKDIR /app

COPY package.json pnpm-lock.yaml ./
# Copy pnpm store from fetcher stage to enable offline prod install
COPY --from=fetcher /root/.local/share/pnpm/store /root/.local/share/pnpm/store
RUN pnpm install --prod --frozen-lockfile --offline

COPY --from=builder /app/dist ./dist

# Run as non-root user for security
USER node

# RTP PORT RANGE REQUIREMENT:
# This service opens one UDP socket per concurrent call for RTP media.
# The UDP port range is controlled by RTP_PORT_MIN and RTP_PORT_MAX env vars.
# Default range: 10000–10099 (100 ports, supports ~50 concurrent calls at 2 ports each).
#
# LINUX PRODUCTION (docker-compose.yml uses network_mode: host):
#   No EXPOSE needed — container uses host network stack directly.
#   All ports are accessible without explicit publishing.
#
# macOS / Windows (Docker Desktop — network_mode: host is silently ignored):
#   Add this to docker-compose.override.yml:
#     ports:
#       - "10000-10099:10000-10099/udp"
#   WARNING: Restrict the range to ≤100 ports — publishing large ranges
#   causes Docker Desktop's port proxy to stall on startup.
EXPOSE 10000-10099/udp

CMD ["node", "dist/index.js"]
