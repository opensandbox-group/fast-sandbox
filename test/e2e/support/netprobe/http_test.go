package netprobe

import (
	"reflect"
	"strings"
	"testing"
)

func TestHTTPGetCommandUsesPositionalArguments(t *testing.T) {
	host := `10.0.0.1; echo unsafe`
	command := HTTPGetCommand(host, 8080)

	if got, want := command[len(command)-2:], []string{host, "8080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("HTTP probe arguments = %q, want %q", got, want)
	}
	if command[0] != "timeout" || command[2] != "bash" || command[3] != "-c" {
		t.Fatalf("HTTP probe prefix = %q, want timeout followed by bash -c", command[:4])
	}
	script := command[4]
	if !strings.Contains(script, `exec 3<>"/dev/tcp/$1/$2" || exit 1`) ||
		!strings.Contains(script, `>&3 || exit 1`) ||
		!strings.Contains(script, `cat <&3 || true`) {
		t.Fatalf("HTTP probe script does not preserve connect/write failures while accepting peer-close after a response: %q", script)
	}
}

func TestHTTPGetInNetNSCommandPrefixesNamespaceExecution(t *testing.T) {
	command := HTTPGetInNetNSCommand("sandbox-ns", "10.0.0.2", 18080)
	wantPrefix := []string{"ip", "netns", "exec", "sandbox-ns", "timeout"}

	if !reflect.DeepEqual(command[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("network namespace probe prefix = %q, want %q", command[:len(wantPrefix)], wantPrefix)
	}
}
