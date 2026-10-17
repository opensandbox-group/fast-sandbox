package resourceadmission

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultProcSelfCgroup = "/proc/self/cgroup"
	defaultCgroupRoot     = "/sys/fs/cgroup"
	defaultProcMeminfo    = "/proc/meminfo"
)

type CgroupReader struct {
	cgroupPath    string
	meminfoPath   string
	fallbackLimit uint64
}

func NewSelfCgroupReader(fallbackLimit uint64) (*CgroupReader, error) {
	return NewCgroupReader(defaultProcSelfCgroup, defaultCgroupRoot, defaultProcMeminfo, fallbackLimit)
}

func NewCgroupPathReader(cgroupPath, meminfoPath string, fallbackLimit uint64) (*CgroupReader, error) {
	if cgroupPath == "" {
		return nil, errors.New("cgroup path is required")
	}
	if _, err := os.Stat(filepath.Join(cgroupPath, "memory.current")); err != nil {
		return nil, fmt.Errorf("resolve cgroup v2 memory controller at %s: %w", cgroupPath, err)
	}
	return &CgroupReader{cgroupPath: cgroupPath, meminfoPath: meminfoPath, fallbackLimit: fallbackLimit}, nil
}

func NewCgroupReader(procCgroupPath, cgroupRoot, meminfoPath string, fallbackLimit uint64) (*CgroupReader, error) {
	data, err := os.ReadFile(procCgroupPath)
	if err != nil {
		return nil, fmt.Errorf("read self cgroup: %w", err)
	}
	relative := ""
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			relative = strings.TrimPrefix(filepath.Clean("/"+parts[2]), "/")
			break
		}
	}
	if relative == "" && !strings.Contains(string(data), "0::/") {
		return nil, errors.New("cgroup v2 unified hierarchy is unavailable")
	}
	path := filepath.Join(cgroupRoot, relative)
	return NewCgroupPathReader(path, meminfoPath, fallbackLimit)
}

func (r *CgroupReader) Read() (Sample, error) {
	current, err := readUintFile(filepath.Join(r.cgroupPath, "memory.current"))
	if err != nil {
		return Sample{}, err
	}
	limitData, err := os.ReadFile(filepath.Join(r.cgroupPath, "memory.max"))
	if err != nil {
		return Sample{}, fmt.Errorf("read memory.max: %w", err)
	}
	limitText := strings.TrimSpace(string(limitData))
	limit := r.fallbackLimit
	if limitText != "max" {
		limit, err = strconv.ParseUint(limitText, 10, 64)
		if err != nil {
			return Sample{}, fmt.Errorf("parse memory.max: %w", err)
		}
	}
	if r.fallbackLimit > 0 && limit > r.fallbackLimit {
		limit = r.fallbackLimit
	}
	if limit == 0 {
		return Sample{}, errors.New("memory.max is unlimited and no runtime limit fallback is available")
	}
	events, err := readKeyValues(filepath.Join(r.cgroupPath, "memory.events"))
	if err != nil {
		return Sample{}, err
	}
	psi, err := readPSIAvg10(filepath.Join(r.cgroupPath, "memory.pressure"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Sample{}, err
	}
	nodeAvailable, err := readNodeAvailableRatio(r.meminfoPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Sample{}, err
	}
	return Sample{
		MemoryCurrentBytes: current, MemoryLimitBytes: limit, OOMKills: events["oom_kill"],
		MemoryPSIAvg10: psi, NodeAvailableRatio: nodeAvailable,
	}, nil
}

func readUintFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return value, nil
}

func readKeyValues(path string) (map[string]uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	values := make(map[string]uint64)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s %s: %w", filepath.Base(path), fields[0], parseErr)
		}
		values[fields[0]] = value
	}
	return values, scanner.Err()
}

func readPSIAvg10(path string) (float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "avg10=") {
				return strconv.ParseFloat(strings.TrimPrefix(field, "avg10="), 64)
			}
		}
	}
	return 0, scanner.Err()
}

func readNodeAvailableRatio(path string) (float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	values := make(map[string]uint64)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			return 0, parseErr
		}
		values[fields[0]] = value
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	total := values["MemTotal:"]
	available := values["MemAvailable:"]
	if total == 0 || available == 0 {
		return 0, nil
	}
	return float64(available) / float64(total), nil
}
