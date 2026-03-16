package basicvalidation

import (
	"os"
	"testing"

	"fast-sandbox/test/e2e/support/suiteenv"
)

var testSuite = suiteenv.New()

func TestMain(m *testing.M) {
	os.Exit(testSuite.Env().Run(m))
}

func TestBasicValidationSuiteBootstrap(t *testing.T) {
	t.Skip("basicvalidation suite cases are not implemented yet")
}
