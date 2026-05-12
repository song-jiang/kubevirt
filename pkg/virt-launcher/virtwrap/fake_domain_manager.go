/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package virtwrap

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	cmdv1 "kubevirt.io/kubevirt/pkg/handler-launcher-com/cmd/v1"
	cmdclient "kubevirt.io/kubevirt/pkg/virt-handler/cmd-client"
	"kubevirt.io/kubevirt/pkg/virt-launcher/metadata"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/stats"
)

// DomainEventNotifier is the interface for sending domain events to virt-handler.
// This avoids an import cycle with the notify-client package.
type DomainEventNotifier interface {
	SendDomainEvent(event watch.Event) error
}

// FakeDomainManager implements DomainManager without libvirt/QEMU.
// It simulates VM lifecycle and migration for testing infrastructure
// concerns (networking, storage, scheduling) on clusters without
// hardware virtualization support.
//
// Core methods:
//   - SyncVMI: creates domain, starts `sleep infinity` process, writes PID file, emits Added event
//   - KillVMI/DeleteVMI/SignalShutdownVMI: sets Shutoff state, kills fake process, emits Modified event
//   - PauseVMI/UnpauseVMI: toggles Paused/Running state
//   - ListAllDomains: returns the fake domain (or empty before SyncVMI)
//   - MarkGracefulShutdownVMI: sets grace period metadata
//
// Migration methods:
//   - MigrateVMI (source): sets migration metadata, spawns goroutine that sleeps ~3s
//     then transitions to Shutoff/Migrated
//   - PrepareMigrationTarget: creates domain on target, spawns goroutine that sleeps ~2s
//     then transitions to Running
//   - FinalizeVirtualMachineMigration: no-op
//   - CancelVMIMigration: sets abort status in migration metadata
//
// All other methods return zero values or "not supported in simulation mode".
type FakeDomainManager struct {
	mu             sync.Mutex
	domain         *api.Domain
	metadataCache  *metadata.Cache
	notifier       DomainEventNotifier
	events         chan watch.Event
	runWithNonRoot bool
	stopChan       chan struct{}

	// Fake process management
	fakeCmd *exec.Cmd
	fakePID int
	pidFile string
}

// NewFakeDomainManager creates a FakeDomainManager that simulates VM lifecycle.
func NewFakeDomainManager(
	metadataCache *metadata.Cache,
	runWithNonRoot bool,
	stopChan chan struct{},
) *FakeDomainManager {
	return &FakeDomainManager{
		metadataCache:  metadataCache,
		runWithNonRoot: runWithNonRoot,
		stopChan:       stopChan,
	}
}

// SetNotifier sets the notifier used to send domain events to virt-handler.
func (f *FakeDomainManager) SetNotifier(notifier DomainEventNotifier) {
	f.notifier = notifier
}

// SetEventsChan sets the local events channel used by waitForDomainUUID
// and waitForFinalNotify in virt-launcher main().
func (f *FakeDomainManager) SetEventsChan(events chan watch.Event) {
	f.events = events
}

func (f *FakeDomainManager) pidDir() string {
	if f.runWithNonRoot {
		return "/run/libvirt/qemu/run"
	}
	return "/run/libvirt/qemu"
}

func (f *FakeDomainManager) startFakeProcess(domainName string) {
	cmd := exec.Command("sleep", "infinity")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Log.Reason(err).Error("Failed to start fake process for simulation mode")
		return
	}
	f.fakeCmd = cmd
	f.fakePID = cmd.Process.Pid

	dir := f.pidDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Log.Reason(err).Errorf("Failed to create PID directory %s", dir)
		return
	}
	f.pidFile = filepath.Join(dir, domainName+".pid")
	if err := os.WriteFile(f.pidFile, []byte(strconv.Itoa(f.fakePID)), 0644); err != nil {
		log.Log.Reason(err).Errorf("Failed to write PID file %s", f.pidFile)
		return
	}

	// Reap the process in the background
	go cmd.Wait()

	log.Log.Infof("Simulation mode: started fake process PID %d, pidFile %s", f.fakePID, f.pidFile)
}

func (f *FakeDomainManager) killFakeProcess() {
	if f.fakeCmd != nil && f.fakeCmd.Process != nil {
		if err := syscall.Kill(f.fakePID, syscall.SIGTERM); err != nil {
			log.Log.Reason(err).Warningf("Failed to kill fake process PID %d", f.fakePID)
		}
		f.fakeCmd = nil
		f.fakePID = 0
	}
	if f.pidFile != "" {
		os.Remove(f.pidFile)
		f.pidFile = ""
	}
}

func (f *FakeDomainManager) emitEvent(eventType watch.EventType) {
	if f.domain == nil {
		return
	}
	domainCopy := f.domain.DeepCopy()
	event := watch.Event{
		Type:   eventType,
		Object: domainCopy,
	}

	if f.notifier != nil {
		if err := f.notifier.SendDomainEvent(event); err != nil {
			log.Log.Reason(err).Error("Simulation mode: failed to send domain event via notifier")
		}
	}
	if f.events != nil {
		select {
		case f.events <- event:
		default:
			log.Log.Warning("Simulation mode: events channel full, dropping event")
		}
	}
}

// --- DomainManager interface implementation ---

func (f *FakeDomainManager) SyncVMI(vmi *v1.VirtualMachineInstance, allowEmulation bool, options *cmdv1.VirtualMachineOptions) (*api.DomainSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	log.Log.Object(vmi).Info("Simulation mode: SyncVMI called")

	domainName := api.VMINamespaceKeyFunc(vmi)

	if f.domain == nil {
		f.domain = &api.Domain{}
		// ObjectMeta.Name must be the VMI name (not namespace_name) so the
		// domain cache key (namespace/name) matches the VMI informer key.
		f.domain.ObjectMeta.Name = vmi.Name
		f.domain.ObjectMeta.Namespace = vmi.Namespace
		f.domain.ObjectMeta.UID = vmi.UID
		// Spec.Name is the libvirt domain name (namespace_name format),
		// used for PID files and internal identification.
		f.domain.Spec.Name = domainName
		f.domain.Spec.UUID = string(vmi.UID)
		f.domain.Spec.Metadata.KubeVirt.UID = vmi.UID

		f.metadataCache.UID.Set(vmi.UID)

		f.startFakeProcess(domainName)

		f.domain.SetState(api.Running, api.ReasonUnknown)
		f.emitEvent(watch.Added)

		// Send GARP to announce the VM's presence on the network
		if err := sendGARP("eth0"); err != nil {
			log.Log.Reason(err).Warning("Simulation mode: failed to send GARP on initial boot")
		}
	}

	return &f.domain.Spec, nil
}

func (f *FakeDomainManager) PauseVMI(vmi *v1.VirtualMachineInstance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	log.Log.Object(vmi).Info("Simulation mode: PauseVMI called")
	if f.domain != nil {
		f.domain.SetState(api.Paused, api.ReasonPausedUser)
		f.emitEvent(watch.Modified)
	}
	return nil
}

func (f *FakeDomainManager) UnpauseVMI(vmi *v1.VirtualMachineInstance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	log.Log.Object(vmi).Info("Simulation mode: UnpauseVMI called")
	if f.domain != nil {
		f.domain.SetState(api.Running, api.ReasonUnknown)
		f.emitEvent(watch.Modified)
	}
	return nil
}

func (f *FakeDomainManager) FreezeVMI(_ *v1.VirtualMachineInstance, _ int32) error {
	return nil
}

func (f *FakeDomainManager) UnfreezeVMI(_ *v1.VirtualMachineInstance) error {
	return nil
}

func (f *FakeDomainManager) ResetVMI(_ *v1.VirtualMachineInstance) error {
	return nil
}

func (f *FakeDomainManager) SoftRebootVMI(_ *v1.VirtualMachineInstance) error {
	return nil
}

func (f *FakeDomainManager) KillVMI(vmi *v1.VirtualMachineInstance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	log.Log.Object(vmi).Info("Simulation mode: KillVMI called")

	if f.domain == nil {
		return nil
	}

	f.domain.SetState(api.Shutoff, api.ReasonDestroyed)
	now := metav1.Now()
	f.domain.ObjectMeta.DeletionTimestamp = &now

	f.killFakeProcess()
	f.emitEvent(watch.Modified)
	return nil
}

func (f *FakeDomainManager) DeleteVMI(vmi *v1.VirtualMachineInstance) error {
	return f.KillVMI(vmi)
}

func (f *FakeDomainManager) SignalShutdownVMI(vmi *v1.VirtualMachineInstance) error {
	return f.KillVMI(vmi)
}

func (f *FakeDomainManager) MarkGracefulShutdownVMI() {
	log.Log.Info("Simulation mode: MarkGracefulShutdownVMI called")
	gracePeriod := api.GracePeriodMetadata{
		MarkedForGracefulShutdown: boolPtr(true),
	}
	f.metadataCache.GracePeriod.Store(gracePeriod)
}

func (f *FakeDomainManager) ListAllDomains() ([]*api.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.domain == nil {
		return []*api.Domain{}, nil
	}
	return []*api.Domain{f.domain.DeepCopy()}, nil
}

func (f *FakeDomainManager) MigrateVMI(vmi *v1.VirtualMachineInstance, _ *cmdclient.MigrationOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	log.Log.Object(vmi).Info("Simulation mode: MigrateVMI (source) called")

	// Initialize migration metadata - get UID from VMI status, same as real code
	migrationUID := vmi.Status.MigrationState.MigrationUID
	if vmi.Status.MigrationState.SourceState != nil {
		migrationUID = vmi.Status.MigrationState.SourceState.MigrationUID
	}
	now := metav1.Now()
	migrationMetadata := api.MigrationMetadata{
		UID:            migrationUID,
		StartTimestamp: &now,
		Mode:           v1.MigrationPreCopy,
	}
	f.metadataCache.Migration.Store(migrationMetadata)

	// Simulate migration in background
	go f.simulateMigration(vmi)
	return nil
}

func (f *FakeDomainManager) simulateMigration(vmi *v1.VirtualMachineInstance) {
	// Simulate migration taking ~3 seconds
	select {
	case <-time.After(3 * time.Second):
	case <-f.stopChan:
		return
	}

	// Mark migration as completed
	now := metav1.Now()
	f.metadataCache.Migration.WithSafeBlock(func(md *api.MigrationMetadata, initialized bool) {
		md.EndTimestamp = &now
	})

	// Transition source to Shutoff/Migrated
	f.mu.Lock()
	if f.domain != nil {
		f.domain.SetState(api.Shutoff, api.ReasonMigrated)
	}
	f.killFakeProcess()
	f.mu.Unlock()

	log.Log.Object(vmi).Info("Simulation mode: migration completed on source, domain shutoff")

	f.emitEvent(watch.Modified)
}

func (f *FakeDomainManager) PrepareMigrationTarget(vmi *v1.VirtualMachineInstance, allowEmulation bool, options *cmdv1.VirtualMachineOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	log.Log.Object(vmi).Info("Simulation mode: PrepareMigrationTarget called")

	domainName := api.VMINamespaceKeyFunc(vmi)

	// Create domain on target
	f.domain = &api.Domain{}
	// ObjectMeta.Name must be the VMI name (not namespace_name) so the
	// domain cache key (namespace/name) matches the VMI informer key.
	f.domain.ObjectMeta.Name = vmi.Name
	f.domain.ObjectMeta.Namespace = vmi.Namespace
	f.domain.ObjectMeta.UID = vmi.UID
	// Spec.Name is the libvirt domain name (namespace_name format).
	f.domain.Spec.Name = domainName
	f.domain.Spec.UUID = string(vmi.UID)
	f.domain.Spec.Metadata.KubeVirt.UID = vmi.UID

	f.metadataCache.UID.Set(vmi.UID)

	// Initialize migration metadata on target
	now := metav1.Now()
	f.metadataCache.Migration.Store(api.MigrationMetadata{
		UID:            vmi.Status.MigrationState.MigrationUID,
		StartTimestamp: &now,
		Mode:           v1.MigrationPreCopy,
	})

	// Simulate target receiving the VM in the background
	go f.simulateTargetReceive(vmi, domainName)
	return nil
}

func (f *FakeDomainManager) simulateTargetReceive(vmi *v1.VirtualMachineInstance, domainName string) {
	// Wait for simulated migration data transfer
	select {
	case <-time.After(2 * time.Second):
	case <-f.stopChan:
		return
	}

	f.mu.Lock()
	f.domain.SetState(api.Running, api.ReasonUnknown)
	f.startFakeProcess(domainName)
	f.mu.Unlock()

	// Mark migration complete on target
	now := metav1.Now()
	f.metadataCache.Migration.WithSafeBlock(func(md *api.MigrationMetadata, initialized bool) {
		md.EndTimestamp = &now
	})

	log.Log.Object(vmi).Info("Simulation mode: target received VM, domain running")

	// Send GARP to announce the VM's presence after migration.
	// This simulates what a real VM does when it resumes on the target node,
	// allowing network agents (e.g., Calico Felix) to detect migration completion.
	if err := sendGARP("eth0"); err != nil {
		log.Log.Reason(err).Warning("Simulation mode: failed to send GARP after migration")
	}

	f.emitEvent(watch.Added)
}

func (f *FakeDomainManager) GetDomainStats() (*stats.DomainStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.domain == nil {
		return &stats.DomainStats{}, nil
	}
	return &stats.DomainStats{
		Name: f.domain.Spec.Name,
		UUID: f.domain.Spec.UUID,
	}, nil
}

func (f *FakeDomainManager) CancelVMIMigration(vmi *v1.VirtualMachineInstance) error {
	log.Log.Object(vmi).Info("Simulation mode: CancelVMIMigration called")
	now := metav1.Now()
	f.metadataCache.Migration.WithSafeBlock(func(md *api.MigrationMetadata, initialized bool) {
		md.AbortStatus = string(v1.MigrationAbortSucceeded)
		md.EndTimestamp = &now
		md.Failed = true
		md.FailureReason = "Migration cancelled"
	})
	return nil
}

func (f *FakeDomainManager) FinalizeVirtualMachineMigration(vmi *v1.VirtualMachineInstance, options *cmdv1.VirtualMachineOptions) error {
	log.Log.Object(vmi).Info("Simulation mode: FinalizeVirtualMachineMigration called (no-op)")
	return nil
}

// --- Stubs for unsupported operations ---

func (f *FakeDomainManager) GetGuestInfo() v1.VirtualMachineInstanceGuestAgentInfo {
	return v1.VirtualMachineInstanceGuestAgentInfo{}
}

func (f *FakeDomainManager) GetUsers() []v1.VirtualMachineInstanceGuestOSUser {
	return nil
}

func (f *FakeDomainManager) GetFilesystems() []v1.VirtualMachineInstanceFileSystem {
	return nil
}

func (f *FakeDomainManager) HotplugHostDevices(_ *v1.VirtualMachineInstance) error {
	return nil
}

func (f *FakeDomainManager) InterfacesStatus() []api.InterfaceStatus {
	return nil
}

func (f *FakeDomainManager) GetGuestOSInfo() *api.GuestOSInfo {
	return nil
}

func (f *FakeDomainManager) Exec(_, _ string, _ []string, _ int32) (string, error) {
	return "", fmt.Errorf("not supported in simulation mode")
}

func (f *FakeDomainManager) GuestPing(_ string) error {
	return fmt.Errorf("not supported in simulation mode")
}

func (f *FakeDomainManager) MemoryDump(_ *v1.VirtualMachineInstance, _ string) error {
	return fmt.Errorf("not supported in simulation mode")
}

func (f *FakeDomainManager) BackupVirtualMachine(_ *v1.VirtualMachineInstance, _ *backupv1.BackupOptions) error {
	return fmt.Errorf("not supported in simulation mode")
}

func (f *FakeDomainManager) RedefineCheckpoint(_ *v1.VirtualMachineInstance, _ *backupv1.BackupCheckpoint) (bool, error) {
	return false, fmt.Errorf("not supported in simulation mode")
}

func (f *FakeDomainManager) GetQemuVersion() (string, error) {
	return "simulation-0.0.0", nil
}

func (f *FakeDomainManager) UpdateVCPUs(_ *v1.VirtualMachineInstance, _ *cmdv1.VirtualMachineOptions) error {
	return nil
}

func (f *FakeDomainManager) GetSEVInfo() (*v1.SEVPlatformInfo, error) {
	return nil, fmt.Errorf("not supported in simulation mode")
}

func (f *FakeDomainManager) GetLaunchMeasurement(_ *v1.VirtualMachineInstance) (*v1.SEVMeasurementInfo, error) {
	return nil, fmt.Errorf("not supported in simulation mode")
}

func (f *FakeDomainManager) InjectLaunchSecret(_ *v1.VirtualMachineInstance, _ *v1.SEVSecretOptions) error {
	return fmt.Errorf("not supported in simulation mode")
}

func (f *FakeDomainManager) UpdateGuestMemory(_ *v1.VirtualMachineInstance) error {
	return nil
}

func (f *FakeDomainManager) GetDomainDirtyRateStats(_ time.Duration) (*stats.DomainStatsDirtyRate, error) {
	return &stats.DomainStatsDirtyRate{}, nil
}

func (f *FakeDomainManager) GetScreenshot(_ *v1.VirtualMachineInstance) (*cmdv1.ScreenshotResponse, error) {
	return nil, fmt.Errorf("not supported in simulation mode")
}

// --- GARP support ---

// sendGARP sends a Gratuitous ARP on the specified interface to announce
// the pod's IP/MAC to the network. This simulates what a real VM does
// when it boots or resumes after migration, allowing network agents
// (e.g., Calico Felix) to detect the VM is ready to receive traffic.
func sendGARP(ifaceName string) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get interface %s: %v", ifaceName, err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return fmt.Errorf("failed to get addresses for %s: %v", ifaceName, err)
	}

	var srcIP net.IP
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			srcIP = ip4
			break
		}
	}
	if srcIP == nil {
		return fmt.Errorf("no IPv4 address found on %s", ifaceName)
	}

	srcMAC := iface.HardwareAddr

	// Build Gratuitous ARP packet (ARP reply announcing our IP/MAC)
	// Ethernet header (14 bytes): dst(6) + src(6) + ethertype(2)
	// ARP payload (28 bytes): htype(2) + ptype(2) + hlen(1) + plen(1) + oper(2) + sha(6) + spa(4) + tha(6) + tpa(4)
	pkt := make([]byte, 42)

	// Ethernet header
	copy(pkt[0:6], net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) // dst: broadcast
	copy(pkt[6:12], srcMAC)                                              // src: our MAC
	binary.BigEndian.PutUint16(pkt[12:14], 0x0806)                       // ethertype: ARP

	// ARP payload
	binary.BigEndian.PutUint16(pkt[14:16], 1)                              // hardware type: Ethernet
	binary.BigEndian.PutUint16(pkt[16:18], 0x0800)                         // protocol type: IPv4
	pkt[18] = 6                                                            // hardware address length
	pkt[19] = 4                                                            // protocol address length
	binary.BigEndian.PutUint16(pkt[20:22], 2)                              // operation: ARP reply
	copy(pkt[22:28], srcMAC)                                               // sender hardware address
	copy(pkt[28:32], srcIP.To4())                                          // sender protocol address
	copy(pkt[32:38], net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) // target hardware address
	copy(pkt[38:42], srcIP.To4())                                          // target protocol address (same as sender for GARP)

	// Open raw socket
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ARP)))
	if err != nil {
		return fmt.Errorf("failed to open raw socket: %v", err)
	}
	defer syscall.Close(fd)

	// Build sockaddr_ll for sending
	addr := syscall.SockaddrLinklayer{
		Protocol: htons(syscall.ETH_P_ARP),
		Ifindex:  iface.Index,
		Halen:    6,
	}
	copy(addr.Addr[:6], net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	if err := syscall.Sendto(fd, pkt, 0, &addr); err != nil {
		return fmt.Errorf("failed to send GARP: %v", err)
	}

	log.Log.Infof("Simulation mode: sent GARP on %s (IP=%s, MAC=%s)", ifaceName, srcIP, srcMAC)
	return nil
}

// htons converts a uint16 from host to network byte order.
func htons(v uint16) uint16 {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], v)
	return *(*uint16)(unsafe.Pointer(&buf[0]))
}

// --- Helpers ---

func boolPtr(b bool) *bool {
	return &b
}
