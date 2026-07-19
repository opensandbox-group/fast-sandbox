package cmd

import (
	"fmt"
	"strings"

	fastpathv1 "fast-sandbox/api/proto/v1"
	"fast-sandbox/pkg/sandboxclient"

	"github.com/spf13/viper"
)

func newExecdAdapter(client fastpathv1.FastPathServiceClient) (*sandboxclient.ExecdAdapter, error) {
	adapterName := strings.ToLower(strings.TrimSpace(viper.GetString("adapter")))
	if adapterName == "" {
		adapterName = "execd"
	}
	if adapterName != "execd" && adapterName != "opensandbox-execd" {
		return nil, fmt.Errorf("adapter %q does not implement the OpenSandbox Execd command/file protocol", adapterName)
	}
	resolver := &sandboxclient.EndpointResolver{
		Control: client, DefaultNamespace: viper.GetString("namespace"), ProxyBaseURL: viper.GetString("proxy-endpoint"),
	}
	return &sandboxclient.ExecdAdapter{Resolver: resolver}, nil
}

func sandboxReference(name string) sandboxclient.SandboxRef {
	return sandboxclient.SandboxRef{Name: name, Namespace: viper.GetString("namespace")}
}
