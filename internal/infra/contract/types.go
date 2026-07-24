// Package contract defines runtime-neutral Infra Component observations that
// can cross the Fastlet/runtime adapter boundary.
package contract

import infracatalog "fast-sandbox/internal/catalog/infra"

type ServiceEndpoint struct {
	Component string                      `json:"component"`
	Name      string                      `json:"name"`
	Port      uint32                      `json:"port"`
	Readiness infracatalog.ReadinessProbe `json:"readiness"`
	Required  bool                        `json:"required"`
	Init      infracatalog.InstanceInit   `json:"init"`
}

type ComponentDiagnostic struct {
	Component string `json:"component"`
	Service   string `json:"service"`
	Required  bool   `json:"required"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
}
