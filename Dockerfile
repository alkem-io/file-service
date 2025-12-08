# Stage 1: Build the application
FROM node:22-bookworm AS build

WORKDIR /usr/src/app

# Copy package files first to leverage Docker cache
COPY package*.json ./

# Install all dependencies (including devDependencies) for building
RUN npm ci

# Copy the rest of the application source code
COPY . .

# Build the application
RUN npm run build

# Remove devDependencies to reduce image size
RUN npm prune --production

# Stage 2: Create the production image
# Use distroless image for smaller size and better security
FROM gcr.io/distroless/nodejs22-debian12

WORKDIR /usr/src/app

# Copy built application from the build stage
COPY --from=build --chown=nonroot:nonroot /usr/src/app/dist ./dist
COPY --from=build --chown=nonroot:nonroot /usr/src/app/node_modules ./node_modules

# Copy necessary configuration files
COPY --from=build --chown=nonroot:nonroot /usr/src/app/config.yml ./config.yml

# Set environment variables
ARG ENV_ARG=production
ENV NODE_ENV=${ENV_ARG}
ENV NODE_OPTIONS="--max-old-space-size=4096"

# Explicitly define the user (good practice)
USER nonroot

# Expose the application port
EXPOSE 4003

# Start the application
CMD ["dist/main.js"]
