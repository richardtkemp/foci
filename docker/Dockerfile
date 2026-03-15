# Multi-stage build for foci
# Stage 1: Build Go binaries
FROM golang:1.24-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
	gcc libc6-dev make git \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN make all

# Stage 2: Minimal runtime image
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
	ca-certificates sqlite3 tmux jq curl \
	&& rm -rf /var/lib/apt/lists/*

# Create foci user and secrets group
RUN useradd --system --home-dir /home/foci --create-home --shell /bin/bash foci \
	&& groupadd foci-secrets \
	&& usermod -aG foci-secrets foci

# Install binaries
COPY --from=builder /src/bin/foci-gw /usr/local/bin/foci-gw
COPY --from=builder /src/bin/foci /usr/local/bin/foci
COPY --from=builder /src/bin/foci-call /usr/local/bin/foci-call

# Stage shared files outside /home/foci so they can be synced into
# the persistent volume on every container start (not just first run).
COPY --from=builder /src/shared/ /opt/foci-shared/
COPY --from=builder /src/docs/ /opt/foci-shared/docs/

# Stage import directories (place .md files in docker/character/ or docker/memory/)
COPY docker/character/ /opt/foci-import/character/
COPY docker/memory/ /opt/foci-import/memory/

# Copy entrypoint
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Record build commit
ARG GIT_COMMIT=unknown
RUN echo "${GIT_COMMIT}" > /opt/foci-shared/.foci-commit

# Entrypoint runs as root, then drops to foci via setpriv
# (replicates systemd SupplementaryGroups + AmbientCapabilities)
WORKDIR /home/foci

ENTRYPOINT ["/entrypoint.sh"]
