package diagnostics

import (
	"fmt"

	"fast-sandbox/test/e2e/support/suiteenv"
)

const DefaultControllerNamespace = suiteenv.DefaultControllerNamespace

type Target struct {
	Namespace string
	PodName   string
	Selector  string
}

func ControllerLogsTarget() Target {
	return Target{
		Namespace: DefaultControllerNamespace,
		Selector:  "app=fast-sandbox-controller",
	}
}

func PodLogsCommand(target Target, tail int) []string {
	return []string{
		"logs",
		target.PodName,
		"-n",
		target.Namespace,
		fmt.Sprintf("--tail=%d", tail),
	}
}
