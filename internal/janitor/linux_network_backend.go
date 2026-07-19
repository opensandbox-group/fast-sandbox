package janitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
)

type LinuxNetworkBackend struct {
	stateRoot string
	driver    fastletnetwork.Driver
}

func NewLinuxNetworkBackend(stateRoot string, driver fastletnetwork.Driver) *LinuxNetworkBackend {
	return &LinuxNetworkBackend{stateRoot: stateRoot, driver: driver}
}

func (*LinuxNetworkBackend) Name() ResourceBackend { return BackendLinuxNetwork }

func (b *LinuxNetworkBackend) Scan(ctx context.Context) ([]ResourceIdentity, error) {
	if b.stateRoot == "" || b.driver == nil {
		return nil, errors.New("Linux network state root and driver are required")
	}
	entries, err := os.ReadDir(b.stateRoot)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var resources []ResourceIdentity
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		podUID := entry.Name()
		store := fastletnetwork.NewFileStateStore(filepath.Join(b.stateRoot, podUID))
		slots, err := store.LoadAll(ctx)
		if err != nil {
			return nil, fmt.Errorf("load network state for Pod %s: %w", podUID, err)
		}
		for _, slot := range slots {
			if slot.OwnerPodUID != podUID {
				return nil, fmt.Errorf("network slot %s owner Pod UID does not match its state directory", slot.ID)
			}
			resources = append(resources, networkResource(slot))
		}
	}
	return resources, nil
}

func (b *LinuxNetworkBackend) Cleanup(ctx context.Context, expected ResourceIdentity) error {
	if b.stateRoot == "" || b.driver == nil {
		return errors.New("Linux network state root and driver are required")
	}
	if expected.NetworkStatePodUID == "" || expected.NetworkSlotID == "" {
		return errors.New("network resource is missing Pod UID or slot ID")
	}
	store := fastletnetwork.NewFileStateStore(filepath.Join(b.stateRoot, expected.NetworkStatePodUID))
	slots, err := store.LoadAll(ctx)
	if err != nil {
		return err
	}
	var current *fastletnetwork.Slot
	for _, slot := range slots {
		if slot.ID == expected.NetworkSlotID {
			current = slot
			break
		}
	}
	if current == nil {
		return nil
	}
	if !sameResourceFence(expected, networkResource(current)) {
		return errors.New("network slot identity changed before cleanup")
	}
	if err := b.driver.Destroy(ctx, current); err != nil {
		return err
	}
	if err := store.Delete(ctx, current.ID); err != nil {
		return err
	}
	_ = os.Remove(store.Root())
	return nil
}

func networkResource(slot *fastletnetwork.Slot) ResourceIdentity {
	if slot == nil {
		return ResourceIdentity{}
	}
	return ResourceIdentity{
		Backend: BackendLinuxNetwork, ResourceID: slot.OwnerPodUID + "/" + slot.ID,
		FastletPodUID: slot.OwnerPodUID, FastletPodName: slot.OwnerPodName, FastletPodNamespace: slot.OwnerNamespace,
		SandboxUID:    slot.Owner.SandboxUID, SandboxName: slot.Owner.SandboxName, SandboxNamespace: slot.Owner.SandboxNamespace,
		InstanceGeneration: slot.Owner.InstanceGeneration, AssignmentAttempt: slot.Owner.AssignmentAttempt,
		CreatedAt: slot.CreatedAt, NetworkSlotID: slot.ID, NetworkStatePodUID: slot.OwnerPodUID,
	}
}
