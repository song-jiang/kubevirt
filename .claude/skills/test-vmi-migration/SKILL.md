---
name: test-vmi-migration
description: Create a VMI in simulation mode, trigger live migration, verify it succeeds, and check that Calico Felix detects the GARP and transitions the endpoint through the live migration state machine.
---

## Overview

This skill runs an end-to-end live migration test on a KIND cluster with KubeVirt simulation mode enabled. It creates a VMI, migrates it, and verifies both KubeVirt migration success and Calico Felix GARP detection.

## Prerequisites

- KIND cluster running with KubeVirt deployed in simulation mode (`simulationMode: true`)
- Calico installed as the CNI
- KUBECONFIG set or available at the standard Calico test location
- At least 2 worker nodes available

## Step 1: Set KUBECONFIG

Determine the KUBECONFIG path. Common locations:
- `$KUBECONFIG` environment variable
- `/home/song/go/src/github.com/projectcalico/calico/hack/test/kind/kind-kubeconfig.yaml`

Verify cluster access:
```bash
kubectl get nodes
```

## Step 2: Check and Increase inotify Limits

KubeVirt components (especially virt-handler) require high inotify limits. Check and increase on all KIND nodes if needed:

```bash
for node in $(kubectl get nodes -o name | cut -d/ -f2); do
  echo "=== $node ==="
  current_watches=$(docker exec $node sysctl -n fs.inotify.max_user_watches)
  current_instances=$(docker exec $node sysctl -n fs.inotify.max_user_instances)
  echo "  max_user_watches=$current_watches max_user_instances=$current_instances"
  if [ "$current_watches" -lt 1048576 ] || [ "$current_instances" -lt 8192 ]; then
    echo "  -> Increasing limits"
    docker exec $node sysctl -w fs.inotify.max_user_watches=1048576
    docker exec $node sysctl -w fs.inotify.max_user_instances=8192
  else
    echo "  -> OK"
  fi
done
```

If limits were increased and KubeVirt is already deployed, restart virt-handler pods:
```bash
kubectl delete pods -n kubevirt -l kubevirt.io=virt-handler
```

## Step 3: Clean Up Any Previous Test Resources

Delete any leftover VMI and migration objects from prior runs:
```bash
kubectl delete vmim migration1 -n default --ignore-not-found
kubectl delete vmi vm1 -n default --ignore-not-found
```

Wait for virt-launcher pods to terminate:
```bash
kubectl get pods -n default -l kubevirt.io=virt-launcher
```

## Step 4: Create the VMI

Create a VMI with `emptyDisk` (not containerDisk) and the bridge migration annotation:

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  name: vm1
  namespace: default
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
```

Wait for the VMI to reach `Running` phase:
```bash
kubectl get vmi vm1 -n default
```

## Step 5: Verify Simulation Mode

Check the virt-launcher compute container logs to confirm simulation mode and build iteration:
```bash
kubectl logs -n default <virt-launcher-pod> -c compute | grep "build iteration"
```

Expected output:
```
Simulation mode: FakeDomainManager created (build iteration N)
```

Also verify GARP was sent on initial boot:
```bash
kubectl logs -n default <virt-launcher-pod> -c compute | grep "sent GARP"
```

## Step 6: Record Source Node

Note the node the VMI is running on — this is the migration source:
```bash
kubectl get vmi vm1 -n default -o jsonpath='{.status.nodeName}'
```

## Step 7: Trigger Migration

Create a VirtualMachineInstanceMigration:

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstanceMigration
metadata:
  name: migration1
  namespace: default
spec:
  vmiName: vm1
```

## Step 8: Verify Migration Completes

Wait ~15 seconds, then check:

```bash
# Migration phase should be Succeeded
kubectl get vmim migration1 -n default -o jsonpath='{.status.phase}'

# VMI should have moved to a different node
kubectl get vmi vm1 -n default -o jsonpath='{.status.nodeName}'

# Migration state should show completed
kubectl get vmi vm1 -n default -o jsonpath='{.status.migrationState.completed}'
```

**Expected results:**
- Migration phase: `Succeeded`
- VMI node: different from the source node recorded in Step 6
- Migration completed: `true`

## Step 9: Verify GARP on Target

Find the target virt-launcher pod (the one on the new node) and check its logs:

```bash
kubectl get pods -n default -l kubevirt.io=virt-launcher -o wide
kubectl logs -n default <target-virt-launcher-pod> -c compute | grep -E "GARP|FinalizeVirtualMachine"
```

**Expected output:**
```
Simulation mode: FinalizeVirtualMachineMigration called
Simulation mode: sent GARP on eth0-nic (IP=<pod-ip>, MAC=<mac>)
```

The GARP should be sent on `eth0-nic` (bridge binding mode) or `eth0` (no bridge). The key is no "network is down" error.

## Step 10: Verify Felix Detected the GARP

Find the calico-node pod on the **target** node and check Felix logs around the migration time:

```bash
# Find calico-node pod on target node
kubectl get pods -n calico-system -l k8s-app=calico-node -o wide | grep <target-node>

# Check Felix logs for migration state machine transitions
kubectl logs -n calico-system <calico-node-pod> -c calico-node | grep -E "LiveMigration|GARP|live_migration|IPAM owner"
```

**Expected Felix log sequence:**
1. `LiveMigrationCalculator: LiveMigration created/updated` — Felix sees the migration CRD
2. `Live migration state transition from=Base to=Target` — endpoint assigned TARGET role
3. `Detected GARP/RARP packet on workload interface` — Felix captured the GARP
4. `Live migration state transition from=Target to=Live` — GARP triggers Target->Live transition
5. `Starting IPAM owner attribute swap for live migration` — Felix swaps IPAM ownership

## Step 11: Clean Up

```bash
kubectl delete vmim migration1 -n default --ignore-not-found
kubectl delete vmi vm1 -n default --ignore-not-found
```

## Troubleshooting

| Problem | Cause | Fix |
|---|---|---|
| VMI stuck in `Scheduling` | No schedulable worker nodes | Check `kubectl get nodes` and node taints |
| Migration rejected `InterfaceNotLiveMigratable` | Missing bridge migration annotation | Add `kubevirt.io/allow-pod-bridge-network-live-migration: "true"` |
| Migration stuck at `TargetReady` | Old virt-launcher image without metadata fix | Purge images: `docker exec <node> crictl rmi <image:tag>` on all nodes, restart virt-handler |
| GARP fails "network is down" | Old code sending on `eth0` (dummy) instead of `eth0-nic` | Rebuild and push virt-launcher with latest code |
| Felix doesn't show GARP detection | Felix not watching the right interface or GARP not reaching host | Check interface name in Felix logs, verify `eth0-nic` is UP |
| Build iteration doesn't match | KIND cached old image | Purge old image on all nodes: `docker exec <node> crictl rmi <image:tag>` |
