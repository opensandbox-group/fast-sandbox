package controlplane

import (
	"os"
	"testing"

	"fast-sandbox/test/e2e/support/suiteenv"
)

var testSuite = suiteenv.New(suiteenv.WithNamespacePrefix("fsb-e2e-controlplane"))

func TestMain(m *testing.M) {
	os.Exit(testSuite.Env().Run(m))
}
