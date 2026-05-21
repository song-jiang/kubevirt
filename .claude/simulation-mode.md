# KubeVirt Simulation Mode

## Purpose

Simulation mode enables testing of Calico IPAM and route management during KubeVirt live migration on regular Kubernetes clusters **without nested virtualization**. The entire KubeVirt control plane runs authentically — CRDs, virt-controller, virt-handler, migration state machine, gRPC communication — but the VM execution layer (libvirt/QEMU) is replaced with a `FakeDomainManager` that tracks state in memory.

## Design

The key architectural insight is that the `DomainManager` interface (`pkg/virt-launcher/virtwrap/manager.go`, 38 methods) cleanly separates KubeVirt's control logic from libvirt. We implement `FakeDomainManager` alongside the existing `LibvirtDomainManager`, gated by a `--simulation-mode` flag on the virt-launcher binary.

### What is exercised authentically

- Kubernetes pod scheduling and lifecycle
- Calico/CNI IPAM (IP allocation/release as pods come and go during migration)
- Calico route management (BGP route announcements/withdrawals when pods move between nodes)
- Calico Felix live migration state machine (`Base -> Target -> Live`) triggered by GARP detection
- KubeVirt CRD reconciliation (virt-controller)
- Full migration state machine (`Pending -> Scheduling -> Scheduled -> PreparingTarget -> TargetReady -> Running -> Succeeded`)
- virt-handler source/target migration controllers
- gRPC communication between virt-handler and virt-launcher

### What is faked

- VM execution (no QEMU process, just `sleep infinity` for PID file)
- Migration data transfer (timed state transitions instead of memory copy)
- Guest agent queries (return empty/error)
- Domain XML / libvirt interaction

## Files Modified

| File | Change |
|---|---|
| `staging/src/kubevirt.io/api/core/v1/types.go` | Add `SimulationMode` field to `DeveloperConfiguration` |
| `pkg/virt-config/virt-config.go` | Add `SimulationMode()` accessor |
| `pkg/virt-controller/services/template.go` | Pass `--simulation-mode` flag to virt-launcher; skip device resource requests |
| `cmd/virt-launcher/virt-launcher.go` | Add `--simulation-mode` flag; branch `main()` to skip libvirt and use `FakeDomainManager` |
| `pkg/virt-handler/vm.go` | Defer source orphan cleanup until migration finalization completes |
| `pkg/virt-handler/controller.go` | Skip `/dev/kvm` and device ownership in simulation mode |
| `pkg/virt-launcher/virtwrap/fake_domain_manager.go` | **New file**: `FakeDomainManager` implementation |
| `hack/dockerized` | Fix docker credential injection into builder container |

Files NOT modified: virt-controller migration logic, virt-handler migration source/target controllers, DomainManager interface, notifier, process monitor.

## FakeDomainManager Implementation

### State management

- Tracks a single `*api.Domain` with lifecycle states (`NoState -> Running -> Shutoff`)
- Uses the existing `metadata.Cache` for migration metadata (same as `LibvirtDomainManager`)
- Uses a `notifyclient.Notifier` to push domain events to virt-handler via `SetNotifier()`
- Manages a fake "QEMU" process (`sleep infinity`) to satisfy `ProcessMonitor` PID file checks

### Key method behaviors

| Method | Behavior |
|---|---|
| `SyncVMI` | Create `api.Domain`, set state=Running, start fake process, write PID file, emit Added event, send GARP |
| `ListAllDomains` | Return the fake domain (or empty if not yet synced) |
| `KillVMI` | State=Shutoff (only if Running/Paused), emit Modified event. Skips if already Shutoff to preserve Migrated reason |
| `DeleteVMI` | Set DeletionTimestamp, emit Modified event. Delays 10s for Shutoff/Migrated domains to let migration controllers finish |
| `MigrateVMI` (source) | Set migration metadata on both cache and domain object, spawn goroutine that sleeps ~3s then sets state=Shutoff/Migrated and EndTimestamp |
| `PrepareMigrationTarget` | Create domain on target, set migration metadata, spawn goroutine that sleeps ~2s then sets state=Running, starts fake process, sets EndTimestamp |
| `FinalizeVirtualMachineMigration` | Send Gratuitous ARP on `eth0-nic` |
| `CancelVMIMigration` | Set AbortStatus=Succeeded, Failed=true in migration metadata |
| `GetDomainStats` | Return minimal stats |
| All others | Return zero values / nil / "not supported in simulation mode" |

### Migration metadata: domain object vs metadata cache

This is the most critical implementation detail. virt-handler's migration controllers read migration state from **two different sources**:

1. **`domain.Spec.Metadata.KubeVirt.Migration`** — on the domain object received via domain events. The migration-target controller's `updateStatus()` checks `EndTimestamp` here to call `ackMigrationCompletion()`.

2. **`metadata.Cache.Migration`** — used internally by the cmd-server for gRPC responses.

The `FakeDomainManager` must set migration metadata on **both**. Early versions only set the cache, which caused migration to get stuck at `TargetReady` because `ackMigrationCompletion` was never called.

### GARP (Gratuitous ARP)

In bridge binding mode, the pod's network interfaces are reorganized:
- `eth0` — dummy interface, state DOWN, NOARP — holds the pod IP
- `eth0-nic` — original veth, state UP — bridge port, connected to host
- `k6t-eth0` — bridge device
- `tap0` — tap for QEMU (no carrier in simulation mode)

The GARP must be sent on `eth0-nic` (the live veth) using the IP from `eth0` (the dummy). Sending on `eth0` directly fails with "network is down" because it's a dummy interface.

The GARP is sent in `FinalizeVirtualMachineMigration` (not in `PrepareMigrationTarget`/`simulateTargetReceive`) because during target preparation the pod's network interface may not be fully up yet.

### Source-side migration timing

After the source domain transitions to `Shutoff/Migrated`, the fake process is **NOT** killed immediately. In the real flow, libvirt sets the domain state first, virt-handler processes the state change and updates VMI status, and only then does the normal cleanup path terminate the process. If we kill the process immediately, `ProcessMonitor` detects the death and shuts down virt-launcher before virt-handler can process the migration completion.

### Idempotency

virt-handler may call `MigrateVMI` and `PrepareMigrationTarget` multiple times during a single migration (on re-enqueue). Boolean flags (`migrationStarted`, `targetPreparationDone`) ensure the background goroutine is only spawned once.

### Source orphan cleanup deferral

In `pkg/virt-handler/vm.go`, the VM controller detects `domainMigrated` (Shutoff/Migrated) and calls `deleteVM()`. Without simulation mode's timing delays, this can race with the migration-target controller setting `MigrationState.Completed`. The fix adds a check: if `simulationMode` is true and `MigrationState` exists but is not yet `Completed` or `Failed`, defer cleanup by re-enqueuing after 2 seconds.

## How to Use

### Build and push

```bash
# Build
hack/dockerized "export DOCKER_PREFIX=docker.io/<user> && export DOCKER_TAG=<tag> && hack/bazel-build.sh"

# Push (use direct bazel commands, not push script)
hack/dockerized "bazel run //:push-virt-handler -- --repository docker.io/<user>/virt-handler --tag <tag> && \
  bazel run //:push-virt-launcher -- --repository docker.io/<user>/virt-launcher --tag <tag>"
```

### Deploy on KIND

```bash
# Increase inotify limits on all nodes FIRST
for node in $(kubectl get nodes -o name | cut -d/ -f2); do
  docker exec $node sysctl -w fs.inotify.max_user_watches=1048576
  docker exec $node sysctl -w fs.inotify.max_user_instances=8192
done

# Deploy KubeVirt operator and CR with simulation mode enabled
kubectl apply -f _out/manifests/release/kubevirt-operator.yaml
kubectl apply -f - <<EOF
apiVersion: kubevirt.io/v1
kind: KubeVirt
metadata:
  name: kubevirt
  namespace: kubevirt
spec:
  configuration:
    developerConfiguration:
      simulationMode: true
    imagePullPolicy: IfNotPresent
  imagePullPolicy: IfNotPresent
EOF
```

### Create VMI and migrate

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  name: vm1
  annotations:
    kubevirt.io/allow-pod-bridge-network-live-migration: "true"
spec:
  domain:
    resources:
      requests:
        memory: "64Mi"
    devices:
      disks:
      - name: emptydisk
        disk:
          bus: virtio
  volumes:
  - name: emptydisk
    emptyDisk:
      capacity: 1Gi
---
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstanceMigration
metadata:
  name: migration1
spec:
  vmiName: vm1
```

### Verify Felix detects the migration

On the target node's calico-node pod:
```
14:03:30 LiveMigrationCalculator: LiveMigration created/updated
14:03:31 Live migration state transition from=Base to=Target
14:03:41 Workload interface came up, endpoint status "up"
14:03:43 Detected GARP/RARP packet on workload interface
14:03:43 Live migration state transition from=Target to=Live
14:03:43 Starting IPAM owner attribute swap for live migration
```

## Lessons Learned / Pitfalls

1. **Domain metadata vs cache**: Migration metadata must be set on `f.domain.Spec.Metadata.KubeVirt.Migration` (the domain object), not just the metadata cache. virt-handler reads the domain object, not the cache.

2. **Don't kill the fake process on source after migration**: Let virt-handler process the `Shutoff/Migrated` state change first. Otherwise `ProcessMonitor` terminates virt-launcher prematurely and virt-handler interprets it as a crash.

3. **Separate KillVMI and DeleteVMI**: In production, `virDomainDestroy` (KillVMI) and `virDomainUndefine` (DeleteVMI) are distinct operations. KillVMI must skip when domain is already Shutoff to avoid overwriting the `Migrated` reason. DeleteVMI should delay for Shutoff/Migrated domains to let migration controllers finish.

4. **GARP interface**: Send on `eth0-nic` (the live veth), not `eth0` (dummy interface, state DOWN). The bridge binding setup replaces the original `eth0` with a dummy.

5. **KIND image caching**: KIND nodes cache images by tag. After pushing a new image with the same tag, you must `crictl rmi` on every node before pods will pick up the new version.

6. **Push script limitations**: `hack/bazel-push-images.sh` may not propagate `DOCKER_PREFIX`/`DOCKER_TAG` correctly because `hack/config.sh` sources kubevirtci config with wrong `KUBEVIRTCI_PATH`. Use direct `bazel run //:push-virt-handler -- --repository ... --tag ...` commands instead.

7. **containerDisk won't work**: Simulation mode doesn't set up real disk images. Use `emptyDisk` volumes instead of `containerDisk`.

8. **Bridge migration annotation required**: VMIs with default bridge networking need `kubevirt.io/allow-pod-bridge-network-live-migration: "true"` annotation or the migration webhook rejects with `InterfaceNotLiveMigratable`.

9. **Build iteration tracking**: The `SimBuildIteration` const in `fake_domain_manager.go` is logged at startup to verify which code version is running. Increment it when rebuilding to confirm image updates.
