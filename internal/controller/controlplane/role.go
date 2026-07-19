package controlplane

import "fmt"

type Role string

const (
	RoleFastPath   Role = "fastpath"
	RoleController Role = "controller"
	RoleAll        Role = "all"
)

func ParseRole(value string) (Role, error) {
	role := Role(value)
	switch role {
	case RoleFastPath, RoleController, RoleAll:
		return role, nil
	default:
		return "", fmt.Errorf("unsupported control-plane role %q (want fastpath, controller, or all)", value)
	}
}

func (r Role) RunsFastPath() bool { return r == RoleFastPath || r == RoleAll }

func (r Role) RunsControllers() bool { return r == RoleController || r == RoleAll }

// All is intentionally a single-process development/compatibility mode.
// Production controller replicas use leader election; production FastPath
// replicas never do.
func (r Role) LeaderElection() bool { return r == RoleController }
