package cmd

import (
	fastpathv1 "fast-sandbox/api/proto/v1"
	"fast-sandbox/pkg/sandboxclient"

	"github.com/spf13/viper"
)

func newOpenSandboxExecd(client fastpathv1.FastPathServiceClient) *sandboxclient.OpenSandboxExecd {
	resolver := &sandboxclient.EndpointResolver{
		Control: client, DefaultNamespace: viper.GetString("namespace"), ProxyBaseURL: viper.GetString("proxy-endpoint"),
	}
	return &sandboxclient.OpenSandboxExecd{Resolver: resolver}
}

func sandboxReference(name string) sandboxclient.SandboxRef {
	return sandboxclient.SandboxRef{Name: name, Namespace: viper.GetString("namespace")}
}
