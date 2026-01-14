# ======================
# Builder stage (dev deps)
# ======================
FROM node:22.20.0-bookworm AS builder

WORKDIR /app

# Dependency manifests
COPY package*.json ./

# Deterministic install (includes dev deps)
RUN npm ci

# Build inputs
COPY tsconfig*.json ./
COPY src ./src
COPY config.yml .

# Build TypeScript → dist
RUN npm run build


# ======================
# Prod deps stage (NO dev deps)
# ======================
FROM node:22.20.0-bookworm AS prod-deps

WORKDIR /app

COPY package*.json ./

RUN npm ci --omit=dev \
 && npm cache clean --force


# ======================
# Runtime stage (distroless)
# ======================
FROM gcr.io/distroless/nodejs22-debian12

WORKDIR /app

ENV NODE_ENV=production

# Copy only what is required at runtime
COPY --from=prod-deps /app/node_modules ./node_modules
COPY --from=builder  /app/dist ./dist
COPY --from=builder  /app/config.yml ./config.yml
COPY --from=builder  /app/package.json ./package.json

# Distroless runs as non-root by default
EXPOSE 4003

# No shell, no npm, direct execution
CMD ["dist/main.js"]
