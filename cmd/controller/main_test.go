package main

import (
	"errors"
	"reflect"
	"testing"

	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
)

func TestParseCacheNamespaces(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  map[string]struct{}
	}{
		{name: "empty watches whole cluster", value: "", want: map[string]struct{}{}},
		{name: "single namespace", value: "fast-sandbox-system", want: map[string]struct{}{"fast-sandbox-system": {}}},
		{name: "comma separated with spaces and empties", value: " ns-a , ns-b ,, ,ns-c", want: map[string]struct{}{"ns-a": {}, "ns-b": {}, "ns-c": {}}},
		{name: "duplicate namespaces collapse", value: "ns-a,ns-a", want: map[string]struct{}{"ns-a": {}}},
		{name: "whitespace only", value: " , ", want: map[string]struct{}{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCacheNamespaces(tc.value)
			if len(got) != len(tc.want) {
				t.Fatalf("parseCacheNamespaces(%q) = %v, want %v", tc.value, got, tc.want)
			}
			for name := range tc.want {
				if _, ok := got[name]; !ok {
					t.Errorf("parseCacheNamespaces(%q) missing namespace %q", tc.value, name)
				}
			}
		})
	}
}

func TestSortedCacheNamespaceNames(t *testing.T) {
	configs := map[string]ctrlcache.Config{
		"ns-b": {},
		"ns-a": {},
		"ns-c": {},
	}
	got := sortedCacheNamespaceNames(configs)
	want := []string{"ns-a", "ns-b", "ns-c"}
	if len(got) != len(want) {
		t.Fatalf("sortedCacheNamespaceNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedCacheNamespaceNames() = %v, want %v", got, want)
		}
	}
}

func TestValidateKubeClientTuning(t *testing.T) {
	cases := []struct {
		name  string
		qps   float64
		burst int
		want  error
	}{
		{name: "zero pair keeps defaults", qps: 0, burst: 0, want: nil},
		{name: "positive pair accepted", qps: 100, burst: 200, want: nil},
		{name: "qps without burst rejected", qps: 100, burst: 0, want: errors.New("kube-client-qps and kube-client-burst must be set together")},
		{name: "burst without qps rejected", qps: 0, burst: 200, want: errors.New("kube-client-qps and kube-client-burst must be set together")},
		{name: "negative qps rejected", qps: -1, burst: 200, want: errors.New("kube-client-qps and kube-client-burst must not be negative")},
		{name: "negative burst rejected", qps: 100, burst: -1, want: errors.New("kube-client-qps and kube-client-burst must not be negative")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateKubeClientTuning(tc.qps, tc.burst)
			if tc.want == nil && got != nil {
				t.Fatalf("validateKubeClientTuning(%v, %v) = %v, want nil", tc.qps, tc.burst, got)
			}
			if tc.want != nil && (got == nil || got.Error() != tc.want.Error()) {
				t.Fatalf("validateKubeClientTuning(%v, %v) = %v, want %v", tc.qps, tc.burst, got, tc.want)
			}
		})
	}
}

func TestCacheNamespaceConfigs(t *testing.T) {
	configs, scoped := cacheNamespaceConfigs(nil)
	if scoped || configs != nil {
		t.Fatalf("cacheNamespaceConfigs(nil) = %v, %v; want nil, false", configs, scoped)
	}
	configs, scoped = cacheNamespaceConfigs(map[string]struct{}{})
	if scoped || configs != nil {
		t.Fatalf("cacheNamespaceConfigs(empty) = %v, %v; want nil, false", configs, scoped)
	}
	configs, scoped = cacheNamespaceConfigs(map[string]struct{}{"ns-a": {}, "ns-b": {}})
	if !scoped || len(configs) != 2 {
		t.Fatalf("cacheNamespaceConfigs(set) = %v, %v; want two configs, true", configs, scoped)
	}
	for name, config := range configs {
		if !reflect.DeepEqual(config, ctrlcache.Config{}) {
			t.Errorf("cacheNamespaceConfigs(%q) = %+v; want zero config", name, config)
		}
	}
}

func TestValidateControllerCacheScoping(t *testing.T) {
	const runtimeNamespace = "fast-sandbox-system"
	cases := []struct {
		name            string
		namespaces      map[string]struct{}
		runtimeNSFlag   string
		runsControllers bool
		wantErr         bool
	}{
		{name: "empty set keeps cluster wide", namespaces: map[string]struct{}{}, runsControllers: true, wantErr: false},
		{name: "fastpath role skips check", namespaces: map[string]struct{}{"tenant-a": {}}, runsControllers: false, wantErr: false},
		{name: "controller set includes runtime namespace", namespaces: map[string]struct{}{"tenant-a": {}, runtimeNamespace: {}}, runsControllers: true, wantErr: false},
		{name: "controller set missing runtime namespace", namespaces: map[string]struct{}{"tenant-a": {}}, runsControllers: true, wantErr: true},
		{
			name:            "custom runtime namespace flag honored",
			namespaces:      map[string]struct{}{"custom-runtime": {}},
			runtimeNSFlag:   "custom-runtime",
			runsControllers: true,
			wantErr:         false,
		},
		{
			name:            "custom runtime namespace missing",
			namespaces:      map[string]struct{}{runtimeNamespace: {}},
			runtimeNSFlag:   "custom-runtime",
			runsControllers: true,
			wantErr:         true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateControllerCacheScoping(tc.namespaces, tc.runtimeNSFlag, tc.runsControllers)
			if tc.wantErr && err == nil {
				t.Fatalf("validateControllerCacheScoping(%v, %q, %v) = nil, want error", tc.namespaces, tc.runtimeNSFlag, tc.runsControllers)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateControllerCacheScoping(%v, %q, %v) = %v, want nil", tc.namespaces, tc.runtimeNSFlag, tc.runsControllers, err)
			}
		})
	}
}
