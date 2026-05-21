#!/usr/bin/env bash

set -euo pipefail

# CI script to build and push the 5 core KubeVirt images + generate operator manifests.
#
# Required env vars:
#   DOCKER_PREFIX  — registry + org (e.g. quay.io/tigeradev)
#
# Optional env vars:
#   DOCKER_TAG     — image tag. Defaults to sanitized current branch name (slashes → hyphens).

if [[ -z "${DOCKER_PREFIX:-}" ]]; then
    echo "ERROR: DOCKER_PREFIX is required (e.g. quay.io/tigeradev)" >&2
    exit 1
fi

if [[ -z "${DOCKER_TAG:-}" ]]; then
    DOCKER_TAG=$(git rev-parse --abbrev-ref HEAD | tr '/' '-')
    echo "DOCKER_TAG not set, defaulting to branch name: ${DOCKER_TAG}"
fi

export DOCKER_PREFIX DOCKER_TAG

TARGETS=(virt-operator virt-api virt-controller virt-handler virt-launcher)

echo "=== Building and pushing images ==="
echo "  Registry: ${DOCKER_PREFIX}"
echo "  Tag:      ${DOCKER_TAG}"
echo ""

# Work around an upstream bug in hack/dockerized: it both bind-mounts
# ~/.docker/config.json (readonly) and then tries to docker-cp the same
# file into the container, which fails with "device or resource busy".
# Fix: hide config.json so hack/dockerized skips both the bind mount and
# the copy, then inject the credentials inside the container ourselves.
SETUP_AUTH=""
if [[ -f "${HOME}/.docker/config.json" ]]; then
    DOCKER_CFG_B64=$(base64 -w0 "${HOME}/.docker/config.json")
    mv "${HOME}/.docker/config.json" "${HOME}/.docker/config.json.ci-bak"
    trap 'mv -f "${HOME}/.docker/config.json.ci-bak" "${HOME}/.docker/config.json" 2>/dev/null' EXIT
    SETUP_AUTH="mkdir -p /root/.docker && echo '${DOCKER_CFG_B64}' | base64 -d > /root/.docker/config.json && "
fi

# Build push commands for all targets, run in a single hack/dockerized invocation.
PUSH_CMDS=""
for target in "${TARGETS[@]}"; do
    PUSH_CMDS="${PUSH_CMDS}echo '--- Pushing ${DOCKER_PREFIX}/${target}:${DOCKER_TAG} ---' && bazel run //:push-${target} -- --repository ${DOCKER_PREFIX}/${target} --tag ${DOCKER_TAG} && "
done
# Append manifest generation
PUSH_CMDS="${PUSH_CMDS}echo '=== Generating manifests ===' && DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} hack/bazel-build.sh && hack/manifests.sh"

hack/dockerized "${SETUP_AUTH}${PUSH_CMDS}"

echo ""
echo "=== Done ==="
echo "Images pushed:"
for target in "${TARGETS[@]}"; do
    echo "  ${DOCKER_PREFIX}/${target}:${DOCKER_TAG}"
done
echo "Manifests generated in _out/manifests/"
