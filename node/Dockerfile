# Stage 1 — base: enable pnpm via corepack
FROM node:24-alpine AS base
# CI=true so pnpm v11 doesn't abort on its interactive modules-purge confirmation
# (ERR_PNPM_ABORTED_REMOVE_MODULES_DIR_NO_TTY) in the non-TTY Docker build.
ENV CI=true
RUN corepack enable
WORKDIR /app

# Stage 2 — fetcher: download all packages from lockfile into pnpm store.
# package.json is copied so corepack auto-selects the pinned packageManager version.
# Layer invalidates when either file changes.
FROM base AS fetcher
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN pnpm fetch

# Stage 3 — builder: install from pnpm store (offline), copy source, compile.
FROM fetcher AS builder
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
# --offline: read from the store fetched above; no network required
RUN pnpm install --frozen-lockfile --offline
# `COPY . .` from the ./node context includes dist/ui when the operator UI bundle
# has been staged by `make ui-node` (CI does this before the image build). tsup
# does not clean dist/, so `pnpm build` writes dist/index.js alongside the staged
# dist/ui, and the production stage's `COPY --from=builder /app/dist` ships both.
COPY . .
RUN pnpm build

# Stage 4 — production: minimal runtime image.
# Only production deps and compiled dist/. No TypeScript, no tsx, no tsup.
FROM node:24-alpine AS production
ENV CI=true
RUN corepack enable
WORKDIR /app

COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
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
