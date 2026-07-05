# Builder image for luncur deploy jobs.
# Runs in a K3s Job pod with:
#   - PVC mounted at /data (for logs, sources, push creds)
#   - Required env vars:
#     - LUNCUR_DEPLOY_ID: deploy identifier (for log file naming, source tarball naming)
#     - LUNCUR_IMAGE_REF: full image ref to push (e.g., registry.example.com:5000/myapp:v1)
#     - LUNCUR_REGISTRY_HOST: registry host for auth (Plan D wires this into docker/config.json)
#     - LUNCUR_SOURCE_TYPE: "git" or "tarball"
#     - LUNCUR_GIT_URL: git repo URL (if LUNCUR_SOURCE_TYPE=git)
#     - LUNCUR_GIT_BRANCH: git branch/ref (default "main" if LUNCUR_SOURCE_TYPE=git)
#
# The image:
#   1. Fetches source (git clone or tar extract)
#   2. Detects Dockerfile or uses nixpacks to generate one
#   3. Builds and pushes to registry (with insecure=true for in-cluster registries)
#   4. Logs all output to /data/logs/${LUNCUR_DEPLOY_ID}.log

FROM moby/buildkit:rootless

# The rootless base image runs as USER user (uid 1000); package installs
# need root, so switch for the setup steps and drop back at the end.
USER root

# Install git for cloning source repos (needed for LUNCUR_SOURCE_TYPE=git),
# ca-certificates for HTTPS git operations, and bash — entrypoint.sh uses
# bash-only process substitution for tee-based logging.
RUN apk add --no-cache git ca-certificates bash

# Install nixpacks: download the release binary directly from GitHub.
# This avoids needing curl+bash+curl script; we just fetch the binary.
# Using the static binary from the release assets.
ARG NIXPACKS_VERSION=1.41.0
RUN apk add --no-cache curl && \
    curl -fsSL -o /tmp/nixpacks.tar.gz \
      "https://github.com/railwayapp/nixpacks/releases/download/v${NIXPACKS_VERSION}/nixpacks-v${NIXPACKS_VERSION}-x86_64-unknown-linux-musl.tar.gz" && \
    tar -xzf /tmp/nixpacks.tar.gz -C /usr/local/bin && \
    rm /tmp/nixpacks.tar.gz && \
    apk del curl && \
    chmod +x /usr/local/bin/nixpacks

# Copy entrypoint script into the image.
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Back to the rootless buildkit user — builds must not run as root, and the
# system namespace's PodSecurity baseline still expects the image default.
USER user

# Set the entrypoint to run the build logic.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
