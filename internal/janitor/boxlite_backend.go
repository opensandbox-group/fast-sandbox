package janitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	boxlitestate "fast-sandbox/internal/runtime/boxlite/state"

	"golang.org/x/sys/unix"
)

type BoxLiteBackend struct {
	stateRoot string
}

func NewBoxLiteBackend(stateRoot string) *BoxLiteBackend {
	return &BoxLiteBackend{stateRoot: stateRoot}
}

func (*BoxLiteBackend) Name() ResourceBackend { return BackendBoxLite }

func (b *BoxLiteBackend) Scan(ctx context.Context) ([]ResourceIdentity, error) {
	if b.stateRoot == "" {
		return nil, errors.New("BoxLite state root is required")
	}
	entries, err := os.ReadDir(b.stateRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var resources []ResourceIdentity
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() {
			continue
		}
		home := filepath.Join(b.stateRoot, entry.Name())
		owner, err := readBoxLiteOwner(home)
		if err != nil {
			return nil, fmt.Errorf("read BoxLite home %s owner fence: %w", entry.Name(), err)
		}
		if boxlitestate.SafeSegment(owner.FastletPodUID) != entry.Name() {
			return nil, fmt.Errorf("BoxLite home %s does not match owner Pod UID", entry.Name())
		}
		metadataRoot := filepath.Join(home, boxlitestate.MetadataDirectoryName)
		metadata, err := os.ReadDir(metadataRoot)
		if errors.Is(err, os.ErrNotExist) {
			metadata = nil
		} else if err != nil {
			return nil, err
		}
		found := false
		for _, item := range metadata {
			if item.IsDir() || filepath.Ext(item.Name()) != ".json" {
				continue
			}
			record, err := readBoxLiteRecord(filepath.Join(metadataRoot, item.Name()))
			if err != nil {
				return nil, fmt.Errorf("read BoxLite record %s/%s: %w", entry.Name(), item.Name(), err)
			}
			resource, err := boxLiteResource(entry.Name(), item.Name(), owner, record)
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
			found = true
		}
		if !found {
			resources = append(resources, ResourceIdentity{
				Backend: BackendBoxLite, ResourceID: entry.Name(), FastletPodUID: owner.FastletPodUID,
				CreatedAt: time.Unix(owner.CreatedAt, 0),
			})
		}
	}
	return resources, nil
}

func (b *BoxLiteBackend) Cleanup(_ context.Context, expected ResourceIdentity) error {
	if b.stateRoot == "" {
		return errors.New("BoxLite state root is required")
	}
	homeSegment, recordName, err := parseBoxLiteResourceID(expected.ResourceID)
	if err != nil {
		return err
	}
	home := filepath.Join(b.stateRoot, homeSegment)
	owner, err := readBoxLiteOwner(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner.FastletPodUID != expected.FastletPodUID || boxlitestate.SafeSegment(owner.FastletPodUID) != homeSegment {
		return errors.New("BoxLite home owner fence changed before cleanup")
	}
	lock, err := os.OpenFile(filepath.Join(home, boxlitestate.RuntimeLockFileName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("BoxLite Runtime still owns state lock: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck

	metadataRoot := filepath.Join(home, boxlitestate.MetadataDirectoryName)
	if recordName != "" {
		recordPath := filepath.Join(metadataRoot, recordName)
		record, err := readBoxLiteRecord(recordPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		current, err := boxLiteResource(homeSegment, recordName, owner, record)
		if err != nil {
			return err
		}
		if !sameResourceFence(expected, current) {
			return errors.New("BoxLite Sandbox identity changed before cleanup")
		}
		if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		bundle := filepath.Join(home, boxlitestate.BundleDirectoryName, boxlitestate.SafeSegment(expected.SandboxUID))
		if err := os.RemoveAll(bundle); err != nil {
			return err
		}
	} else {
		hasRecords, err := hasBoxLiteRecords(metadataRoot)
		if err != nil {
			return err
		}
		if hasRecords {
			return errors.New("BoxLite home gained Sandbox records before cleanup")
		}
	}
	hasRecords, err := hasBoxLiteRecords(metadataRoot)
	if err != nil {
		return err
	}
	if !hasRecords {
		return os.RemoveAll(home)
	}
	return nil
}

func readBoxLiteOwner(home string) (boxlitestate.OwnerRecord, error) {
	file, err := os.Open(filepath.Join(home, boxlitestate.OwnerFileName))
	if err != nil {
		return boxlitestate.OwnerRecord{}, err
	}
	defer file.Close()
	var owner boxlitestate.OwnerRecord
	if err := json.NewDecoder(file).Decode(&owner); err != nil {
		return boxlitestate.OwnerRecord{}, err
	}
	if owner.Version != boxlitestate.Version || owner.FastletPodUID == "" || owner.CreatedAt <= 0 {
		return boxlitestate.OwnerRecord{}, errors.New("invalid BoxLite owner fence")
	}
	return owner, nil
}

func readBoxLiteRecord(path string) (boxlitestate.SandboxRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return boxlitestate.SandboxRecord{}, err
	}
	defer file.Close()
	var record boxlitestate.SandboxRecord
	if err := json.NewDecoder(file).Decode(&record); err != nil {
		return boxlitestate.SandboxRecord{}, err
	}
	if record.Version != boxlitestate.Version || record.Request.Sandbox.SandboxID == "" ||
		record.Request.Sandbox.FastletPodUID == "" || record.CreatedAt <= 0 {
		return boxlitestate.SandboxRecord{}, errors.New("invalid BoxLite Sandbox record")
	}
	return record, nil
}

func boxLiteResource(homeSegment, recordName string, owner boxlitestate.OwnerRecord, record boxlitestate.SandboxRecord) (ResourceIdentity, error) {
	spec := record.Request.Sandbox
	if spec.FastletPodUID != owner.FastletPodUID || recordName != boxlitestate.RecordFileName(spec.SandboxID) {
		return ResourceIdentity{}, errors.New("BoxLite Sandbox record does not match its owner or filename fence")
	}
	sandboxNamespace := spec.ClaimNamespace
	if sandboxNamespace == "" {
		sandboxNamespace = record.Namespace
	}
	return ResourceIdentity{
		Backend: BackendBoxLite, ResourceID: filepath.Join(homeSegment, recordName),
		FastletPodUID: owner.FastletPodUID, FastletPodNamespace: record.Namespace,
		SandboxUID: spec.SandboxID, SandboxName: spec.ClaimName, SandboxNamespace: sandboxNamespace,
		InstanceGeneration: spec.InstanceGeneration, AssignmentAttempt: spec.AssignmentAttempt,
		RouteGeneration: spec.RouteGeneration, CreatedAt: time.Unix(record.CreatedAt, 0),
	}, nil
}

func parseBoxLiteResourceID(resourceID string) (string, string, error) {
	clean := filepath.Clean(resourceID)
	if clean == "." || filepath.IsAbs(clean) || clean != resourceID || strings.Contains(clean, "..") {
		return "", "", errors.New("unsafe BoxLite resource ID")
	}
	parts := strings.Split(clean, string(os.PathSeparator))
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		return "", "", errors.New("invalid BoxLite resource ID")
	}
	if len(parts) == 2 && (parts[1] == "" || filepath.Ext(parts[1]) != ".json") {
		return "", "", errors.New("invalid BoxLite record resource ID")
	}
	record := ""
	if len(parts) == 2 {
		record = parts[1]
	}
	return parts[0], record, nil
}

func hasBoxLiteRecords(metadataRoot string) (bool, error) {
	entries, err := os.ReadDir(metadataRoot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			return true, nil
		}
	}
	return false, nil
}
