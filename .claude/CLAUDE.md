# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Overview

KubeVirt is a Kubernetes extension that enables running virtual machines alongside containers. It follows the operator pattern, extending the Kubernetes API with CRDs for VMs, live migration, snapshots, and more.

**Primary language:** Go 1.24
**Build system:** Bazel (inside Docker via `hack/dockerized`)
**Testing framework:** Ginkgo v2
**Default branch:** `main`
**Module path:** `kubevirt.io/kubevirt`

## Gotchas

- **NEVER** run builds outside the dockerized wrapper — use `hack/dockerized` for all build commands
- **NEVER** modify files under `staging/src/kubevirt.io/client-go/` by hand — they are generated
- **ALWAYS** run builds via Bazel inside the dockerized container, not raw `go build`
- **ALWAYS** use `go.work` workspace — the repo has three modules (root, api, client-go)
- API type changes require OpenAPI spec regeneration (`hack/generate.sh`)

## Architecture

### Component Dependency Order

```
staging/src/kubevirt.io/api/     - CRD type definitions (VM, VMI, migration, etc.)
staging/src/kubevirt.io/client-go/ - Generated API clients
pkg/                              - Shared libraries
cmd/virt-operator/                - Deploys and manages KubeVirt components
cmd/virt-api/                     - Kubernetes API extension server (webhooks, subresources)
cmd/virt-controller/              - Cluster-wide controller (VM/VMI/migration reconciliation)
cmd/virt-handler/                 - Per-node DaemonSet (manages virt-launcher pods)
cmd/virt-launcher/                - Per-VM pod process (wraps libvirt/QEMU)
cmd/virtctl/                      - CLI tool
```

### Key Architectural Concepts

**virt-controller** watches VM/VMI/Migration CRDs and creates/manages pods. It does NOT interact with libvirt directly.

**virt-handler** runs on every node as a DaemonSet. It:
- Watches VMIs scheduled to its node
- Communicates with virt-launcher via gRPC (cmd-server)
- Runs three independent controllers: VM controller (`vm.go`), migration-source (`migration-source.go`), migration-target (`migration-target.go`)

**virt-launcher** runs one per VM pod. It:
- Manages the libvirt/QEMU lifecycle via the `DomainManager` interface
- Exposes a gRPC cmd-server for virt-handler communication
- Monitors the QEMU process via `ProcessMonitor`
- Emits domain events to virt-handler via `notifyclient.Notifier`

**DomainManager interface** (`pkg/virt-launcher/virtwrap/manager.go`) cleanly separates control logic from libvirt. `LibvirtDomainManager` is the production implementation. `FakeDomainManager` is the simulation mode implementation.

### Live Migration Flow

```
virt-controller: creates target pod
virt-handler (target): PrepareMigrationTarget -> domain appears on target
virt-handler (source): MigrateVMI -> libvirt migrates memory/state
virt-handler (target): detects EndTimestamp -> ackMigrationCompletion -> finalizeMigration
virt-handler (target): FinalizeVirtualMachineMigration (reconnect NICs, GARP)
virt-handler (source): detects Shutoff/Migrated -> deleteVM -> cleanup
```

Migration state machine phases: `Pending -> Scheduling -> Scheduled -> PreparingTarget -> TargetReady -> Running -> Succeeded/Failed`

## Essential Build Commands

All builds run inside Docker containers via `hack/dockerized`.

### Building

```bash
# Build all components (recommended)
hack/dockerized "export DOCKER_PREFIX=<registry> && export DOCKER_TAG=<tag> && hack/bazel-build.sh"

# Build specific targets
hack/dockerized "bazel build //cmd/virt-handler:virt-handler"
hack/dockerized "bazel build //cmd/virt-launcher:virt-launcher"
hack/dockerized "bazel build //cmd/virt-controller:virt-controller"
```

### Pushing Images

```bash
# Push individual images (recommended over push script for custom registries)
hack/dockerized "bazel run //:push-virt-handler -- --repository docker.io/<user>/virt-handler --tag <tag>"
hack/dockerized "bazel run //:push-virt-launcher -- --repository docker.io/<user>/virt-launcher --tag <tag>"
hack/dockerized "bazel run //:push-virt-controller -- --repository docker.io/<user>/virt-controller --tag <tag>"
hack/dockerized "bazel run //:push-virt-operator -- --repository docker.io/<user>/virt-operator --tag <tag>"
```

Note: `hack/bazel-push-images.sh` may not correctly propagate `DOCKER_PREFIX`/`DOCKER_TAG` in all environments. Direct `bazel run` commands are more reliable for custom registries.

### Running Tests

```bash
# Unit tests for a specific package
hack/dockerized "bazel test //pkg/virt-handler/..."
hack/dockerized "bazel test //pkg/virt-launcher/virtwrap/..."

# All unit tests (slow)
hack/dockerized "bazel test //pkg/..."

# Run specific test by name
hack/dockerized "bazel test //pkg/virt-handler:go_default_test --test_filter=TestSomething"

# Generate manifests
hack/dockerized "DOCKER_PREFIX=<registry> DOCKER_TAG=<tag> hack/bazel-build.sh && hack/manifests.sh"
```

### Code Generation

```bash
# Regenerate all generated code (after API type changes)
hack/dockerized "hack/generate.sh"

# Update OpenAPI spec
hack/dockerized "hack/update-swagger.sh"
```

## Key Files and Locations

### API Types
- `staging/src/kubevirt.io/api/core/v1/types.go` - VM, VMI, Migration CRD types
- `staging/src/kubevirt.io/api/core/v1/deepcopy_generated.go` - Generated deep copy
- `pkg/virt-config/virt-config.go` - Cluster configuration accessors

### Component Entry Points
- `cmd/virt-launcher/virt-launcher.go` - Launcher main (libvirt setup, cmd-server)
- `cmd/virt-handler/virt-handler.go` - Handler main
- `cmd/virt-controller/virt-controller.go` - Controller main

### Core Logic
- `pkg/virt-launcher/virtwrap/manager.go` - `DomainManager` interface (38 methods)
- `pkg/virt-launcher/virtwrap/live-migration-source.go` - Source-side migration
- `pkg/virt-launcher/virtwrap/live-migration-target.go` - Target-side migration
- `pkg/virt-handler/vm.go` - VM controller (domain lifecycle)
- `pkg/virt-handler/migration-source.go` - Migration source controller
- `pkg/virt-handler/migration-target.go` - Migration target controller
- `pkg/virt-controller/services/template.go` - Pod template generation

### Networking
- `pkg/network/setup/netpod/netpod.go` - Pod network setup (bridge binding)
- `pkg/network/link/names.go` - Interface naming (k6t-eth0, eth0-nic, tap0)
- `pkg/network/namescheme/networknamescheme.go` - Network name schemes

### Simulation Mode
- `pkg/virt-launcher/virtwrap/fake_domain_manager.go` - FakeDomainManager
- `pkg/virt-handler/vm.go` - Source orphan cleanup deferral
- `pkg/virt-handler/controller.go` - Device ownership skip

### Build Configuration
- `hack/dockerized` - Dockerized build wrapper
- `hack/config.sh` - Build configuration
- `hack/common.sh` - Shared shell utilities
- `WORKSPACE` / `BUILD.bazel` - Bazel workspace definition

## Code Conventions

### Go Import Order

Three groups separated by blank lines: stdlib, external, kubevirt-internal:
```go
import (
    "fmt"
    "net"

    "k8s.io/api/core/v1"
    "libvirt.org/go/libvirt"

    v1 "kubevirt.io/api/core/v1"
    "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)
```

### Logging

Use the structured logger from `kubevirt.io/client-go/log`:
```go
log.Log.Object(vmi).Info("message")
log.Log.Object(vmi).Reason(err).Warning("something failed")
log.Log.Infof("formatted %s", value)
```

### API Domain Types

Domain (libvirt) types are in `pkg/virt-launcher/virtwrap/api/schema.go`. These use XML tags (not JSON) since they map to libvirt domain XML. The `Metadata.KubeVirt.Migration` field on the domain object is what virt-handler reads for migration state — it is separate from the `metadata.Cache` used internally by virt-launcher.

## Development Cluster

KubeVirt uses kubevirtci for development clusters:

```bash
# Start a development cluster
export KUBEVIRT_PROVIDER=k8s-1.30
make cluster-up

# Sync code to running cluster
make cluster-sync

# Access the cluster
export KUBECONFIG=$(hack/cluster/cli.sh kubeconfig)
```

### KIND Cluster Setup

When deploying KubeVirt on a KIND cluster, you **must** increase inotify limits on every node before deploying KubeVirt. Without this, virt-handler and other components will fail with inotify watch exhaustion errors:

```bash
for node in $(kubectl get nodes -o name | cut -d/ -f2); do
  docker exec $node sysctl -w fs.inotify.max_user_watches=1048576
  docker exec $node sysctl -w fs.inotify.max_user_instances=8192
done
```

When updating images with the same tag, you must purge the old image from each node first — KIND caches images by tag and won't re-pull:
```bash
for node in $(kubectl get nodes -o name | cut -d/ -f2); do
  docker exec $node crictl rmi <image:tag>
done
```

## Simulation Mode

The simulation mode replaces libvirt/QEMU with a `FakeDomainManager` for testing network plugins (e.g., Calico) during live migration without nested virtualization. See [simulation-mode.md](simulation-mode.md) for full design, implementation details, and lessons learned.
