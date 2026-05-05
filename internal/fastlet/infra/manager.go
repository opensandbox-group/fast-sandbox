package infra

import (
	"fmt"
	"os"
	"path/filepath"
)

type Plugin struct {
	Name          string `json:"name"`
	BinName       string `json:"binName"`
	ContainerPath string `json:"containerPath"`
	IsWrapper     bool   `json:"isWrapper"`
}

type Manager struct {
	podInfraPath  string
	hostInfraPath string
	plugins       []Plugin
}

func NewManager(podPath string) *Manager {
	m := &Manager{
		podInfraPath: podPath,
		plugins: []Plugin{
			{
				Name:          "system-helper",
				BinName:       "fs-helper",
				ContainerPath: "/.fs/helper",
				IsWrapper:     true,
			},
		},
	}
	m.discoverHostPath()
	return m
}

func (m *Manager) discoverHostPath() {

	podUID := os.Getenv("POD_UID")
	if podUID == "" {
		fmt.Printf("Warning: POD_UID not set, infra injection might fail\n")
		return
	}

	// host path: /var/lib/kubelet/pods/<UID>/volumes/kubernetes.io~empty-dir/<VOLUME_NAME>
	m.hostInfraPath = fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~empty-dir/infra-tools", podUID)
	fmt.Printf("Determined internal node path for infra: %s\n", m.hostInfraPath)
}

func (m *Manager) GetHostPath(binName string) string {
	if m.hostInfraPath == "" {
		return ""
	}
	return filepath.Join(m.hostInfraPath, binName)
}

func (m *Manager) GetPlugins() []Plugin {
	return m.plugins
}
