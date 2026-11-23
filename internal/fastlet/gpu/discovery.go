package gpu

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/api/resource"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// AssignedDevices returns only devices allocated by kubelet to this container.
// UUID strategy and exclusive device-plugin allocation are required.
func AssignedDevices(ctx context.Context, socket, namespace, pod, container string, expected int) ([]string, error) {
	if socket == "" || namespace == "" || pod == "" || container == "" || expected <= 0 {
		return nil, fmt.Errorf("GPU discovery requires kubelet socket, Pod/container identity and expected count")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	target := url.URL{Scheme: "unix", Path: socket}
	conn, err := grpc.NewClient(target.String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	response, err := podresources.NewPodResourcesListerClient(conn).List(ctx, &podresources.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("query kubelet GPU allocation: %w", err)
	}
	return devicesForContainer(response, namespace, pod, container, expected)
}

// ValidateAssignedDevices checks the actual assigned UUIDs before creating any
// leases. Unconstrained Pools do not require nvidia-smi or perform extra probes.
func ValidateAssignedDevices(ctx context.Context, ids []string, requirements *apiv1alpha2.GPUDeviceRequirements) error {
	if requirements == nil {
		return nil
	}
	if len(ids) == 0 {
		return fmt.Errorf("GPU requirements cannot be checked without assigned devices")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=uuid,name,memory.total", "--format=csv,noheader", "--id="+strings.Join(ids, ",")).Output()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("query assigned NVIDIA GPU properties: %w", ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("query assigned NVIDIA GPU properties: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("query assigned NVIDIA GPU properties (nvidia-smi must be available in the Fastlet container): %w", err)
	}
	return validateDeviceProperties(ids, requirements, output)
}

var gpuProductCharacters = regexp.MustCompile(`[^A-Za-z0-9-_. ]`)

func validateDeviceProperties(ids []string, requirements *apiv1alpha2.GPUDeviceRequirements, output []byte) error {
	remaining := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, exists := remaining[id]; exists || !uuidPattern.MatchString(id) {
			return fmt.Errorf("invalid assigned GPU UUID %q", id)
		}
		remaining[id] = struct{}{}
	}
	reader := csv.NewReader(bytes.NewReader(output))
	reader.FieldsPerRecord = 3
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("parse assigned NVIDIA GPU properties: %w", err)
	}
	for _, row := range records {
		id := strings.TrimSpace(row[0])
		if _, exists := remaining[id]; !exists {
			return fmt.Errorf("GPU properties contain unassigned or duplicate UUID %q", id)
		}
		if requirements.Model != "" {
			model := strings.TrimSpace(row[1])
			if model == "" || strings.EqualFold(model, "N/A") || strings.EqualFold(model, "[N/A]") {
				return fmt.Errorf("assigned GPU %s reports an unavailable model", id)
			}
			// Match GFD's exclusive full-GPU product label normalization.
			productLabel := strings.Join(strings.Fields(gpuProductCharacters.ReplaceAllString(model, "")), "-")
			if productLabel != requirements.Model {
				return fmt.Errorf("assigned GPU %s model %q does not match required model %q", id, model, requirements.Model)
			}
		}
		if err := validateDeviceMemory(id, row[2], requirements.MinimumMemory); err != nil {
			return err
		}
		delete(remaining, id)
	}
	for _, id := range ids {
		if _, missing := remaining[id]; missing {
			return fmt.Errorf("assigned GPU %s is missing from NVIDIA GPU properties", id)
		}
	}
	return nil
}

func validateDeviceMemory(id, reported string, minimum *resource.Quantity) error {
	if minimum == nil {
		return nil
	}
	fields := strings.Fields(reported)
	if len(fields) != 2 || fields[1] != "MiB" {
		return fmt.Errorf("assigned GPU %s reports invalid total memory %q", id, reported)
	}
	memory, err := resource.ParseQuantity(fields[0] + "Mi")
	if err != nil || memory.Sign() <= 0 {
		return fmt.Errorf("assigned GPU %s reports invalid total memory %q", id, reported)
	}
	if memory.Cmp(*minimum) < 0 {
		return fmt.Errorf("assigned GPU %s total memory %s is below required minimum %s", id, memory.String(), minimum.String())
	}
	return nil
}

func devicesForContainer(response *podresources.ListPodResourcesResponse, namespace, pod, container string, expected int) ([]string, error) {
	var assigned []string
	deviceClaims := map[string]int{}
	for _, p := range response.GetPodResources() {
		isTargetPod := p.GetNamespace() == namespace && p.GetName() == pod
		for _, c := range p.GetContainers() {
			isTargetContainer := isTargetPod && c.GetName() == container
			for _, d := range c.GetDevices() {
				if d.GetResourceName() != ResourceName {
					continue
				}
				ids := d.GetDeviceIds()
				for _, id := range ids {
					deviceClaims[id]++
				}
				if isTargetContainer {
					assigned = append(assigned, ids...)
				}
			}
		}
	}
	if len(assigned) != expected {
		return nil, fmt.Errorf("kubelet assigned %d GPUs; expected %d for %s/%s/%s", len(assigned), expected, namespace, pod, container)
	}
	for _, id := range assigned {
		if !uuidPattern.MatchString(id) || deviceClaims[id] != 1 {
			return nil, fmt.Errorf("GPU %q is not a unique full-device UUID allocation; sharing and MIG are unsupported", id)
		}
	}
	slices.Sort(assigned)
	return assigned, nil
}
