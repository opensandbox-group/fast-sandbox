package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	"fast-sandbox/internal/observability"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const reportSchemaVersion = "fast-sandbox-create-load/v1"

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type envList map[string]string

func (values *envList) String() string {
	keys := make([]string, 0, len(*values))
	for key := range *values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+(*values)[key])
	}
	return strings.Join(parts, ",")
}

func (values *envList) Set(value string) error {
	key, item, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return errors.New("env must use KEY=VALUE")
	}
	if *values == nil {
		*values = make(map[string]string)
	}
	(*values)[key] = item
	return nil
}

type config struct {
	Endpoint           string
	Namespace          string
	Pool               string
	Image              string
	Command            string
	Args               []string
	Envs               map[string]string
	WorkingDir         string
	Requests           int
	Concurrency        int
	Rate               float64
	RequestTimeout     time.Duration
	RequestIDPrefix    string
	Cleanup            bool
	CleanupTimeout     time.Duration
	Commit             string
	Environment        string
	Runtime            string
	InfraProfile       string
	ImageState         string
	ImageAffinity      string
	NetworkSlotState   string
	CreatePath         string
	FastPathReplicas   int
	ControllerReplicas int
	ProxyReplicas      int
}

type dimensions struct {
	Runtime            string `json:"runtime"`
	InfraProfile       string `json:"infra_profile"`
	ImageState         string `json:"image_state"`
	ImageAffinity      string `json:"image_affinity"`
	NetworkSlotState   string `json:"network_slot_state"`
	CreatePath         string `json:"create_path"`
	FastPathReplicas   int    `json:"fastpath_replicas"`
	ControllerReplicas int    `json:"controller_replicas"`
	ProxyReplicas      int    `json:"sandbox_proxy_replicas"`
}

type loadShape struct {
	Requests    int     `json:"requests"`
	Concurrency int     `json:"concurrency"`
	RateLimit   float64 `json:"rate_limit_per_second"`
	Timeout     string  `json:"request_timeout"`
}

type latencySummary struct {
	Samples int     `json:"samples"`
	Min     float64 `json:"min"`
	Mean    float64 `json:"mean"`
	P50     float64 `json:"p50"`
	P95     float64 `json:"p95"`
	P99     float64 `json:"p99"`
	Max     float64 `json:"max"`
}

type identitySummary struct {
	UniqueSandboxUIDs     int `json:"unique_sandbox_uids"`
	DuplicateSandboxUIDs  int `json:"duplicate_sandbox_uids"`
	MissingSandboxUIDs    int `json:"missing_sandbox_uids"`
	UniqueSandboxNames    int `json:"unique_sandbox_names"`
	DuplicateSandboxNames int `json:"duplicate_sandbox_names"`
	MissingSandboxNames   int `json:"missing_sandbox_names"`
}

type cleanupReport struct {
	Attempted int            `json:"attempted"`
	Succeeded int            `json:"succeeded"`
	Failed    int            `json:"failed"`
	Codes     map[string]int `json:"grpc_codes,omitempty"`
	Duration  float64        `json:"duration_ms"`
}

type report struct {
	SchemaVersion              string          `json:"schema_version"`
	Commit                     string          `json:"commit"`
	Environment                string          `json:"environment"`
	Endpoint                   string          `json:"endpoint"`
	Namespace                  string          `json:"namespace"`
	Pool                       string          `json:"pool"`
	Image                      string          `json:"image"`
	RequestIDPrefix            string          `json:"request_id_prefix"`
	StartedAt                  time.Time       `json:"started_at"`
	FinishedAt                 time.Time       `json:"finished_at"`
	Duration                   float64         `json:"duration_ms"`
	Dimensions                 dimensions      `json:"dimensions"`
	Load                       loadShape       `json:"load"`
	Succeeded                  int             `json:"succeeded"`
	Failed                     int             `json:"failed"`
	Attempted                  int             `json:"attempted"`
	NotAttempted               int             `json:"not_attempted"`
	FailureRate                float64         `json:"failure_rate"`
	GRPCCodes                  map[string]int  `json:"grpc_codes"`
	CreateRPCLatency           latencySummary  `json:"create_rpc_latency_ms"`
	SuccessfulCreateRPCLatency latencySummary  `json:"successful_create_rpc_latency_ms"`
	Identity                   identitySummary `json:"identity"`
	Cleanup                    *cleanupReport  `json:"cleanup,omitempty"`
}

type fastPathClient interface {
	CreateSandbox(context.Context, *fastpathv1.CreateRequest, ...grpc.CallOption) (*fastpathv1.CreateResponse, error)
	DeleteSandbox(context.Context, *fastpathv1.DeleteRequest, ...grpc.CallOption) (*fastpathv1.DeleteResponse, error)
}

type outcome struct {
	latency   time.Duration
	code      codes.Code
	response  *fastpathv1.CreateResponse
	success   bool
	attempted bool
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(execute(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func execute(ctx context.Context, arguments []string, output, errorOutput io.Writer) int {
	cfg, err := parseConfig(arguments, errorOutput)
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return 2
	}
	traceShutdown, err := observability.Configure(ctx, "fast-sandbox-create-load")
	if err != nil {
		fmt.Fprintln(errorOutput, "configure OpenTelemetry:", err)
		return 1
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := traceShutdown(shutdownContext); err != nil {
			fmt.Fprintln(errorOutput, "flush OpenTelemetry traces:", err)
		}
	}()

	dialContext, dialCancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer dialCancel()
	connection, err := grpc.DialContext(dialContext, cfg.Endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(observability.UnaryClientInterceptor("create-load")),
		grpc.WithBlock(),
	)
	if err != nil {
		fmt.Fprintln(errorOutput, "dial FastPath:", err)
		return 1
	}
	defer connection.Close()

	report := runLoad(ctx, fastpathv1.NewFastPathServiceClient(connection), cfg)
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(errorOutput, "encode report:", err)
		return 1
	}
	if report.Failed > 0 || report.Identity.DuplicateSandboxUIDs > 0 || report.Identity.DuplicateSandboxNames > 0 ||
		report.Identity.MissingSandboxUIDs > 0 || report.Identity.MissingSandboxNames > 0 {
		return 1
	}
	if report.Cleanup != nil && report.Cleanup.Failed > 0 {
		return 1
	}
	return 0
}

func parseConfig(arguments []string, errorOutput io.Writer) (config, error) {
	flags := flag.NewFlagSet("create-load", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	cfg := config{}
	var args stringList
	var envs envList
	flags.StringVar(&cfg.Endpoint, "endpoint", "127.0.0.1:9090", "FastPath gRPC endpoint")
	flags.StringVar(&cfg.Namespace, "namespace", "default", "Sandbox namespace")
	flags.StringVar(&cfg.Pool, "pool", "default-pool", "SandboxPool name")
	flags.StringVar(&cfg.Image, "image", "docker.io/library/alpine:latest", "user image")
	flags.StringVar(&cfg.Command, "command", "/bin/sh", "user command")
	flags.Var(&args, "arg", "user command argument; repeat for multiple arguments")
	flags.Var(&envs, "env", "user environment KEY=VALUE; repeat for multiple values")
	flags.StringVar(&cfg.WorkingDir, "working-dir", "", "user working directory")
	flags.IntVar(&cfg.Requests, "requests", 100, "total Create RPCs")
	flags.IntVar(&cfg.Concurrency, "concurrency", 10, "concurrent clients")
	flags.Float64Var(&cfg.Rate, "rate", 0, "optional global request rate per second; 0 is unlimited")
	flags.DurationVar(&cfg.RequestTimeout, "request-timeout", 30*time.Second, "timeout for dial and each RPC")
	flags.StringVar(&cfg.RequestIDPrefix, "request-id-prefix", "", "stable unique prefix; defaults to a timestamped run ID")
	flags.BoolVar(&cfg.Cleanup, "cleanup", false, "submit declarative deletion for successful creates")
	flags.DurationVar(&cfg.CleanupTimeout, "cleanup-timeout", 2*time.Minute, "total cleanup timeout")
	flags.StringVar(&cfg.Commit, "commit", "unspecified", "tested commit SHA")
	flags.StringVar(&cfg.Environment, "environment", "unspecified", "cluster/hardware/runtime version description")
	flags.StringVar(&cfg.Runtime, "runtime", "unspecified", "runtime profile")
	flags.StringVar(&cfg.InfraProfile, "infra-profile", "unspecified", "InfraProfile")
	flags.StringVar(&cfg.ImageState, "image-state", "unspecified", "warm, cold, or unspecified")
	flags.StringVar(&cfg.ImageAffinity, "image-affinity", "unspecified", "hit, miss, or unspecified")
	flags.StringVar(&cfg.NetworkSlotState, "network-slot-state", "unspecified", "clean, recovered, or unspecified")
	flags.StringVar(&cfg.CreatePath, "create-path", "fastpath", "measured creation path; this tool supports fastpath only")
	flags.IntVar(&cfg.FastPathReplicas, "fastpath-replicas", 0, "observed FastPath replica count")
	flags.IntVar(&cfg.ControllerReplicas, "controller-replicas", 0, "observed Controller replica count")
	flags.IntVar(&cfg.ProxyReplicas, "sandbox-proxy-replicas", 0, "observed Sandbox Proxy replica count")
	if err := flags.Parse(arguments); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if len(args) == 0 {
		args = []string{"-c", "sleep 3600"}
	}
	cfg.Args = append([]string(nil), args...)
	cfg.Envs = map[string]string(envs)
	if cfg.RequestIDPrefix == "" {
		cfg.RequestIDPrefix = "fsb-load-" + time.Now().UTC().Format("20060102t150405.000000000z")
	}
	return cfg, validateConfig(cfg)
}

func validateConfig(cfg config) error {
	if strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.Namespace) == "" || strings.TrimSpace(cfg.Pool) == "" || strings.TrimSpace(cfg.Image) == "" {
		return errors.New("endpoint, namespace, pool, and image are required")
	}
	if cfg.Requests <= 0 || cfg.Requests > 1000000 || cfg.Concurrency <= 0 || cfg.Concurrency > cfg.Requests || cfg.Concurrency > 10000 {
		return errors.New("requests must be between 1 and 1000000; concurrency must be between 1 and min(requests, 10000)")
	}
	if cfg.Rate < 0 || cfg.Rate > 100000 {
		return errors.New("rate must be between 0 and 100000 requests per second")
	}
	if cfg.RequestTimeout <= 0 || cfg.CleanupTimeout <= 0 {
		return errors.New("request-timeout and cleanup-timeout must be positive")
	}
	if cfg.CreatePath != "fastpath" {
		return errors.New("create-load only drives the fastpath RPC; create-path must be fastpath")
	}
	if cfg.FastPathReplicas < 0 || cfg.ControllerReplicas < 0 || cfg.ProxyReplicas < 0 {
		return errors.New("replica counts cannot be negative")
	}
	if !oneOf(cfg.ImageState, "warm", "cold", "unspecified") {
		return errors.New("image-state must be warm, cold, or unspecified")
	}
	if !oneOf(cfg.ImageAffinity, "hit", "miss", "unspecified") {
		return errors.New("image-affinity must be hit, miss, or unspecified")
	}
	if !oneOf(cfg.NetworkSlotState, "clean", "recovered", "unspecified") {
		return errors.New("network-slot-state must be clean, recovered, or unspecified")
	}
	candidateRequestID := requestID(cfg.RequestIDPrefix, cfg.Requests-1)
	if len(candidateRequestID) > 128 {
		return errors.New("request-id-prefix is too long for the 128-byte request_id limit")
	}
	for _, item := range candidateRequestID {
		if item <= 0x20 || item == 0x7f {
			return errors.New("request-id-prefix contains whitespace or control characters")
		}
	}
	return nil
}

func runLoad(ctx context.Context, client fastPathClient, cfg config) report {
	started := time.Now()
	jobs := make(chan int)
	outcomes := make(chan outcome, cfg.Requests)
	var throttle <-chan time.Time
	var ticker *time.Ticker
	if cfg.Rate > 0 {
		interval := time.Duration(float64(time.Second) / cfg.Rate)
		if interval < time.Microsecond {
			interval = time.Microsecond
		}
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
		throttle = ticker.C
	}

	var workers sync.WaitGroup
	for worker := 0; worker < cfg.Concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if throttle != nil {
					select {
					case <-ctx.Done():
						outcomes <- outcome{code: grpcCode(ctx.Err())}
						continue
					case <-throttle:
					}
				}
				requestContext, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
				requestStarted := time.Now()
				response, err := client.CreateSandbox(requestContext, &fastpathv1.CreateRequest{
					Image: cfg.Image, PoolRef: cfg.Pool, Namespace: cfg.Namespace,
					Command: []string{cfg.Command}, Args: append([]string(nil), cfg.Args...),
					Envs: cloneMap(cfg.Envs), WorkingDir: cfg.WorkingDir,
					RequestId: requestID(cfg.RequestIDPrefix, index),
				})
				latency := time.Since(requestStarted)
				cancel()
				if err != nil {
					outcomes <- outcome{latency: latency, code: grpcCode(err), attempted: true}
					continue
				}
				if response == nil {
					outcomes <- outcome{latency: latency, code: codes.Unknown, attempted: true}
					continue
				}
				outcomes <- outcome{latency: latency, code: codes.OK, response: response, success: true, attempted: true}
			}
		}()
	}
	for index := 0; index < cfg.Requests; index++ {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	close(outcomes)

	allLatencies := make([]time.Duration, 0, cfg.Requests)
	successLatencies := make([]time.Duration, 0, cfg.Requests)
	codesByName := make(map[string]int)
	names := make(map[string]int)
	uids := make(map[string]int)
	missingNames, missingUIDs := 0, 0
	createdNames := make([]string, 0, cfg.Requests)
	succeeded, attempted := 0, 0
	for item := range outcomes {
		if item.attempted {
			attempted++
			allLatencies = append(allLatencies, item.latency)
		}
		codesByName[item.code.String()]++
		if !item.success {
			continue
		}
		succeeded++
		successLatencies = append(successLatencies, item.latency)
		if item.response.SandboxUid == "" {
			missingUIDs++
		} else {
			uids[item.response.SandboxUid]++
		}
		if item.response.SandboxName == "" {
			missingNames++
		} else {
			names[item.response.SandboxName]++
			createdNames = append(createdNames, item.response.SandboxName)
		}
	}
	finished := time.Now()
	result := report{
		SchemaVersion: reportSchemaVersion, Commit: cfg.Commit, Environment: cfg.Environment,
		Endpoint: cfg.Endpoint, Namespace: cfg.Namespace, Pool: cfg.Pool, Image: cfg.Image, RequestIDPrefix: cfg.RequestIDPrefix,
		StartedAt: started.UTC(), FinishedAt: finished.UTC(), Duration: milliseconds(finished.Sub(started)),
		Dimensions: dimensions{
			Runtime: cfg.Runtime, InfraProfile: cfg.InfraProfile, ImageState: cfg.ImageState,
			ImageAffinity: cfg.ImageAffinity, NetworkSlotState: cfg.NetworkSlotState, CreatePath: cfg.CreatePath,
			FastPathReplicas: cfg.FastPathReplicas, ControllerReplicas: cfg.ControllerReplicas, ProxyReplicas: cfg.ProxyReplicas,
		},
		Load:      loadShape{Requests: cfg.Requests, Concurrency: cfg.Concurrency, RateLimit: cfg.Rate, Timeout: cfg.RequestTimeout.String()},
		Succeeded: succeeded, Failed: cfg.Requests - succeeded, Attempted: attempted, NotAttempted: cfg.Requests - attempted,
		FailureRate: float64(cfg.Requests-succeeded) / float64(cfg.Requests),
		GRPCCodes:   codesByName, CreateRPCLatency: summarizeLatencies(allLatencies),
		SuccessfulCreateRPCLatency: summarizeLatencies(successLatencies),
		Identity: identitySummary{
			UniqueSandboxUIDs: len(nonEmptyCounts(uids)), DuplicateSandboxUIDs: duplicateCount(uids), MissingSandboxUIDs: missingUIDs,
			UniqueSandboxNames: len(nonEmptyCounts(names)), DuplicateSandboxNames: duplicateCount(names), MissingSandboxNames: missingNames,
		},
	}
	if cfg.Cleanup {
		result.Cleanup = cleanup(ctx, client, cfg, createdNames)
	}
	return result
}

func cleanup(ctx context.Context, client fastPathClient, cfg config, names []string) *cleanupReport {
	started := time.Now()
	cleanupContext, cancel := context.WithTimeout(ctx, cfg.CleanupTimeout)
	defer cancel()
	unique := nonEmptyCounts(countValues(names))
	result := &cleanupReport{Attempted: len(unique), Codes: make(map[string]int)}
	for name := range unique {
		requestContext, requestCancel := context.WithTimeout(cleanupContext, cfg.RequestTimeout)
		response, err := client.DeleteSandbox(requestContext, &fastpathv1.DeleteRequest{SandboxName: name, Namespace: cfg.Namespace})
		requestCancel()
		code := grpcCode(err)
		if err == nil && response != nil && response.Success {
			result.Succeeded++
			result.Codes[codes.OK.String()]++
			continue
		}
		if err == nil {
			code = codes.Unknown
		}
		result.Failed++
		result.Codes[code.String()]++
	}
	result.Duration = milliseconds(time.Since(started))
	return result
}

func requestID(prefix string, index int) string {
	return prefix + "-" + strconv.Itoa(index)
}

func grpcCode(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Code()
	}
	return status.Code(err)
}

func summarizeLatencies(values []time.Duration) latencySummary {
	if len(values) == 0 {
		return latencySummary{}
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	total := time.Duration(0)
	for _, item := range sorted {
		total += item
	}
	return latencySummary{
		Samples: len(sorted), Min: milliseconds(sorted[0]), Mean: milliseconds(total / time.Duration(len(sorted))),
		P50: milliseconds(percentile(sorted, 0.50)), P95: milliseconds(percentile(sorted, 0.95)),
		P99: milliseconds(percentile(sorted, 0.99)), Max: milliseconds(sorted[len(sorted)-1]),
	}
}

func percentile(sorted []time.Duration, quantile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func milliseconds(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) }

func cloneMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func countValues(values []string) map[string]int {
	result := make(map[string]int, len(values))
	for _, value := range values {
		result[value]++
	}
	return result
}

func nonEmptyCounts(values map[string]int) map[string]int {
	result := make(map[string]int, len(values))
	for key, value := range values {
		if key != "" {
			result[key] = value
		}
	}
	return result
}

func duplicateCount(values map[string]int) int {
	duplicates := 0
	for key, value := range values {
		if key != "" && value > 1 {
			duplicates += value - 1
		}
	}
	return duplicates
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}
