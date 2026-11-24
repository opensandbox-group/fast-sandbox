package componentlog

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
)

const (
	ConfigMapName    = "fast-sandbox-logging"
	ConfigMountPath  = "/etc/fast-sandbox/logging"
	ConfigFileName   = "logging.yaml"
	ConfigEnv        = "FAST_SANDBOX_LOG_CONFIG"
	LogDirectory     = "/var/log/fast-sandbox"
	ConfigVolumeName = "component-log-config"
	LogVolumeName    = "component-logs"
	disabledFlag     = "false"
)

type Config struct {
	Directory  string `yaml:"directory"`
	MaxSizeMB  int    `yaml:"maxSizeMB"`
	MaxBackups int    `yaml:"maxBackups"`
	MaxAgeDays int    `yaml:"maxAgeDays"`
	Compress   bool   `yaml:"compress"`
	AlsoStderr bool   `yaml:"alsoStderr"`
}

var outputFlags = map[string]string{
	"log_dir":           "",
	"log_file":          "",
	"log_file_max_size": "0",
	"logtostderr":       disabledFlag,
	"alsologtostderr":   disabledFlag,
	"stderrthreshold":   "4",
	"one_output":        "true",
	"skip_log_headers":  disabledFlag,
}

func InitFlags(flags *flag.FlagSet) {
	if flags == nil {
		flags = flag.CommandLine
	}
	klogFlags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlags)
	klogFlags.VisitAll(func(f *flag.Flag) {
		if _, output := outputFlags[f.Name]; !output {
			flags.Var(f.Value, f.Name, f.Usage)
		}
	})
}

func Configure(component string) (func(), error) {
	path := os.Getenv(ConfigEnv)
	optional := path == ""
	if optional {
		path = filepath.Join(ConfigMountPath, ConfigFileName)
	}
	config, err := loadConfig(path, optional)
	if err != nil {
		return nil, err
	}
	logger, err := newLogger(config, component)
	if err != nil {
		return nil, err
	}
	klogFlags := flag.NewFlagSet("klog-output", flag.ContinueOnError)
	klog.InitFlags(klogFlags)
	for name, value := range outputFlags {
		_ = klogFlags.Set(name, value)
	}
	var output io.Writer = logger
	if config.AlsoStderr {
		output = io.MultiWriter(logger, os.Stderr)
	}
	klog.SetOutput(reportErrors{output: output, diagnostics: os.Stderr})
	return func() {
		klog.Flush()
		if err := logger.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close component log: %v\n", err)
		}
	}, nil
}

func loadConfig(path string, optional bool) (Config, error) {
	config := Config{
		Directory: LogDirectory, MaxSizeMB: 100, MaxBackups: 10,
		MaxAgeDays: 7, Compress: true,
	}
	data, err := os.ReadFile(path)
	if optional && errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read logging config: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode logging config %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("logging config must contain exactly one YAML document")
	}
	if !filepath.IsAbs(config.Directory) {
		return Config{}, errors.New("logging directory must be an absolute path")
	}
	if config.MaxSizeMB <= 0 || config.MaxBackups < 0 || config.MaxAgeDays < 0 {
		return Config{}, errors.New("logging maxSizeMB must be positive; maxBackups and maxAgeDays must be nonnegative")
	}
	return config, nil
}

func newLogger(config Config, component string) (*lumberjack.Logger, error) {
	if err := os.MkdirAll(config.Directory, 0o750); err != nil {
		return nil, fmt.Errorf("create logging directory: %w", err)
	}
	filename := filepath.Join(config.Directory, component+".log")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open component log: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close component log probe: %w", err)
	}
	logger := &lumberjack.Logger{
		Filename: filename, MaxSize: config.MaxSizeMB,
		MaxBackups: config.MaxBackups, MaxAge: config.MaxAgeDays,
		Compress: config.Compress,
	}
	if _, err := logger.Write(nil); err != nil {
		_ = logger.Close()
		return nil, fmt.Errorf("initialize component log: %w", err)
	}
	return logger, nil
}

type reportErrors struct {
	output      io.Writer
	diagnostics io.Writer
}

func (w reportErrors) Write(data []byte) (int, error) {
	n, err := w.output.Write(data)
	if err != nil {
		// klog ignores errors from custom writers, so report them outside klog.
		_, _ = fmt.Fprintf(w.diagnostics, "write component log: %v\n", err)
	}
	return n, err
}
