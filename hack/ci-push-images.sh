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

for target in "${TARGETS[@]}"; do
    echo "--- Pushing ${DOCKER_PREFIX}/${target}:${DOCKER_TAG} ---"
    hack/dockerized "bazel run //:push-${target} -- --repository ${DOCKER_PREFIX}/${target} --tag ${DOCKER_TAG}"
done

echo ""
echo "=== Generating manifests ==="
hack/dockerized "DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} hack/bazel-build.sh && hack/manifests.sh"

echo ""
echo "=== Done ==="
echo "Images pushed:"
for target in "${TARGETS[@]}"; do
    echo "  ${DOCKER_PREFIX}/${target}:${DOCKER_TAG}"
done
echo "Manifests generated in _out/manifests/"
