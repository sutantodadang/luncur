#!/bin/bash
# Entrypoint for luncur builder image.
# Fetches source, detects/generates Dockerfile, builds image, pushes to registry.
# Logs all output to /data/logs/${LUNCUR_DEPLOY_ID}.log via tee.

set -euo pipefail

# ===== Logging Setup =====
# Ensure log directory exists and redirect all output (stdout + stderr) through tee.
# This allows the server to tail the log file in real-time while we also see it on stderr.
LOG="/data/logs/${LUNCUR_DEPLOY_ID}.log"
mkdir -p "$(dirname "$LOG")"

# Redirect both stdout and stderr to tee -a (append mode).
# The tee command copies to the log file and also to the original stdout.
exec > >(tee -a "$LOG")
exec 2>&1

# ===== Banner =====
echo "==== luncur builder ===="
echo "Deploy ID: $LUNCUR_DEPLOY_ID"
echo "Image Ref: $LUNCUR_IMAGE_REF"
echo "Source Type: $LUNCUR_SOURCE_TYPE"
echo "Registry Host: $LUNCUR_REGISTRY_HOST"
echo "Log: $LOG"
echo ""

# ===== Prepare Workspace =====
echo "Preparing workspace..."

if [ "$LUNCUR_SOURCE_TYPE" = "git" ]; then
  # Clone from git repository (shallow clone of the specified branch).
  echo "Cloning from: $LUNCUR_GIT_URL (branch: ${LUNCUR_GIT_BRANCH:-main})"
  git clone --depth 1 --branch "${LUNCUR_GIT_BRANCH:-main}" "$LUNCUR_GIT_URL" /workspace
else
  # Extract from tarball (assumes sources are at /data/sources/${LUNCUR_DEPLOY_ID}.tar.gz).
  echo "Extracting tarball: /data/sources/${LUNCUR_DEPLOY_ID}.tar.gz"
  mkdir -p /workspace
  tar -xzf "/data/sources/${LUNCUR_DEPLOY_ID}.tar.gz" -C /workspace
fi

echo "Workspace ready at /workspace"
echo ""

# ===== Resolve Build Dir =====
# LUNCUR_BUILD_PATH (monorepo support) is an optional repo-relative
# subdirectory to use as the build context/detection dir instead of the
# workspace root, letting one git repo back several apps (e.g. dashboard/
# React + backend/ FastAPI). Unset/empty means "build the workspace root" —
# byte-identical to the pre-monorepo behavior.
BUILD_DIR="/workspace"
if [ -n "${LUNCUR_BUILD_PATH:-}" ]; then
  case "$LUNCUR_BUILD_PATH" in
    /*|*..*) echo "ERROR: invalid LUNCUR_BUILD_PATH: $LUNCUR_BUILD_PATH"; exit 1 ;;
  esac
  BUILD_DIR="/workspace/${LUNCUR_BUILD_PATH}"
  if [ ! -d "$BUILD_DIR" ]; then
    echo "ERROR: build path '$LUNCUR_BUILD_PATH' not found in repository"; exit 1
  fi
  echo "Using build path: $LUNCUR_BUILD_PATH"
fi

# ===== Build Logic =====
echo "Detecting build configuration..."

if [ -f "$BUILD_DIR/Dockerfile" ]; then
  echo "Found Dockerfile in workspace. Using it."
  DOCKERFILE_DIR="$BUILD_DIR"
else
  echo "No Dockerfile found. Generating with nixpacks..."
  nixpacks build "$BUILD_DIR" --out "$BUILD_DIR/.nixpacks"
  DOCKERFILE_DIR="$BUILD_DIR/.nixpacks"
fi

echo ""
echo "Building and pushing image..."

# Per-app BuildKit cache, stored as an image manifest in the embedded
# registry (LUNCUR_CACHE_REF, e.g. luncur-cache/<project>-<app>:buildcache).
# Unset (build_cache=off, or an app's first build) means no cache flags at
# all rather than an empty/broken ref.
CACHE_FLAGS=()
if [ -n "${LUNCUR_CACHE_REF:-}" ]; then
  echo "Using build cache: $LUNCUR_CACHE_REF"
  CACHE_FLAGS+=(
    --import-cache "type=registry,ref=${LUNCUR_CACHE_REF},registry.insecure=true"
    --export-cache "type=registry,mode=max,ref=${LUNCUR_CACHE_REF},registry.insecure=true"
  )
fi

# Use buildctl-daemonless.sh wrapper to run buildkitd + buildctl in rootless mode.
# The wrapper is provided by the moby/buildkit:rootless base image.
#
# Output flags:
#   type=image: build a container image (not just export to tar)
#   name=${LUNCUR_IMAGE_REF}: tag the image with the specified ref
#   push=true: push immediately after building
#   registry.insecure=true: allow unencrypted (http://) pushes to in-cluster registries.
#     This pairs with the K3s registries.yaml config that luncur up writes (Plan D).
#     Without it, buildkit refuses to push to registries without valid HTTPS certs.
#
# NOTE: the wrapper itself execs `buildctl "$@"` — do NOT prefix the args
# with `buildctl`, or the CLI sees `buildctl buildctl build` and exits 3
# with "No help topic for 'buildctl'".
buildctl-daemonless.sh build \
  --frontend dockerfile.v0 \
  --local context="${BUILD_DIR}" \
  --local dockerfile="${DOCKERFILE_DIR}" \
  --output type=image,name="${LUNCUR_IMAGE_REF}",push=true,registry.insecure=true \
  "${CACHE_FLAGS[@]}"

echo ""
echo "==== build complete ===="
echo "Image pushed: $LUNCUR_IMAGE_REF"
