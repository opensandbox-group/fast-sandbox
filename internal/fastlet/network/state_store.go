package network

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var safeSlotID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type FileStateStore struct {
	root string
}

func NewFileStateStore(root string) *FileStateStore {
	return &FileStateStore{root: root}
}

func (s *FileStateStore) Root() string { return s.root }

func (s *FileStateStore) LoadAll(ctx context.Context) ([]*Slot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if errorsIsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read network state directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := make([]*Slot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read network state %s: %w", entry.Name(), err)
		}
		var slot Slot
		if err := json.Unmarshal(data, &slot); err != nil {
			return nil, fmt.Errorf("decode network state %s: %w", entry.Name(), err)
		}
		if !safeSlotID.MatchString(slot.ID) || entry.Name() != slot.ID+".json" {
			return nil, fmt.Errorf("network state %s has invalid slot id %q", entry.Name(), slot.ID)
		}
		result = append(result, &slot)
	}
	return result, nil
}

func (s *FileStateStore) Save(ctx context.Context, slot *Slot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if slot == nil || !safeSlotID.MatchString(slot.ID) {
		return fmt.Errorf("invalid network slot id")
	}
	if err := os.MkdirAll(s.root, 0o750); err != nil {
		return fmt.Errorf("create network state directory: %w", err)
	}
	data, err := json.MarshalIndent(slot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode network state: %w", err)
	}
	data = append(data, '\n')
	target := filepath.Join(s.root, slot.ID+".json")
	temporary, err := os.CreateTemp(s.root, ".slot-*.tmp")
	if err != nil {
		return fmt.Errorf("create network state temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return fmt.Errorf("commit network state: %w", err)
	}
	return syncDirectory(s.root)
}

func (s *FileStateStore) Delete(ctx context.Context, slotID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !safeSlotID.MatchString(slotID) {
		return fmt.Errorf("invalid network slot id")
	}
	err := os.Remove(filepath.Join(s.root, slotID+".json"))
	if errorsIsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDirectory(s.root)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func errorsIsNotExist(err error) bool {
	return err != nil && (os.IsNotExist(err) || err == fs.ErrNotExist)
}
