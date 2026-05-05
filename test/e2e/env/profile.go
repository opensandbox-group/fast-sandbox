package env

import "fmt"

type Profile string

const (
	ProfileBasic    Profile = "basic"
	ProfileGVisor   Profile = "gvisor"
	ProfileKataQemu Profile = "kata-qemu"
	ProfileKataClh  Profile = "kata-clh"
	ProfileKataFc   Profile = "kata-fc"
)

type RuntimeKind string

const (
	RuntimeContainer RuntimeKind = "container"
	RuntimeGVisor    RuntimeKind = "gvisor"
	RuntimeKataQemu  RuntimeKind = "kata-qemu"
	RuntimeKataClh   RuntimeKind = "kata-clh"
	RuntimeKataFc    RuntimeKind = "kata-fc"
)

type ProfileSettings struct {
	ClusterName string
	KindConfig  string
	KindImage   string
	Runtime     RuntimeKind
}

func (p Profile) Settings() (ProfileSettings, error) {
	switch p {
	case ProfileBasic:
		return ProfileSettings{
			ClusterName: "fsb-e2e-basic",
			KindImage:   "kindest/node:v1.27.3",
			Runtime:     RuntimeContainer,
		}, nil
	case ProfileGVisor:
		return ProfileSettings{
			ClusterName: "fsb-e2e-gvisor",
			KindConfig:  "test/e2e/manifests/kind/gvisor.yaml",
			KindImage:   "kindest/node:v1.31.0",
			Runtime:     RuntimeGVisor,
		}, nil
	case ProfileKataQemu:
		return ProfileSettings{
			ClusterName: "fsb-e2e-kata",
			KindConfig:  "test/e2e/manifests/kind/kata.yaml",
			KindImage:   "kindest/node:v1.31.0",
			Runtime:     RuntimeKataQemu,
		}, nil
	case ProfileKataClh:
		return ProfileSettings{
			ClusterName: "fsb-e2e-kata",
			KindConfig:  "test/e2e/manifests/kind/kata.yaml",
			KindImage:   "kindest/node:v1.31.0",
			Runtime:     RuntimeKataClh,
		}, nil
	case ProfileKataFc:
		return ProfileSettings{
			ClusterName: "fsb-e2e-kata",
			KindConfig:  "test/e2e/manifests/kind/kata.yaml",
			KindImage:   "kindest/node:v1.31.0",
			Runtime:     RuntimeKataFc,
		}, nil
	default:
		return ProfileSettings{}, fmt.Errorf("unknown e2e profile %q", p)
	}
}
