package componentlog

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"
)

func TestLoadConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), ConfigFileName)
	defaults, err := loadConfig(missing, true)
	require.NoError(t, err)
	require.Equal(t, Config{Directory: LogDirectory, MaxSizeMB: 100, MaxBackups: 10, MaxAgeDays: 7, Compress: true}, defaults)
	_, err = loadConfig(missing, false)
	require.ErrorIs(t, err, os.ErrNotExist)

	for _, test := range []struct {
		name string
		data string
	}{
		{"unknown field", "maxSizeMb: 2"},
		{"duplicate field", "maxSizeMB: 2\nmaxSizeMB: 3"},
		{"invalid YAML", "directory: ["},
		{"relative directory", "directory: logs"},
		{"empty directory", "directory: ''"},
		{"zero size", "maxSizeMB: 0"},
		{"negative size", "maxSizeMB: -1"},
		{"negative backups", "maxBackups: -1"},
		{"negative age", "maxAgeDays: -1"},
		{"invalid bool", "compress: invalid"},
		{"empty file", ""},
		{"multiple documents", "maxSizeMB: 2\n---\nmaxSizeMB: 3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, test.data)
			_, err := loadConfig(path, false)
			require.Error(t, err)
		})
	}

	path := writeConfig(t, "maxSizeMB: 2\nmaxBackups: 0\nmaxAgeDays: 0\ncompress: false\nalsoStderr: true\n")
	config, err := loadConfig(path, false)
	require.NoError(t, err)
	require.Equal(t, Config{Directory: LogDirectory, MaxSizeMB: 2, AlsoStderr: true}, config)
}

func TestInitFlagsExposesVerbosityButNotOutputOverrides(t *testing.T) {
	state := klog.CaptureState()
	defer state.Restore()
	flags := flag.NewFlagSet("component", flag.ContinueOnError)
	InitFlags(flags)
	for name := range outputFlags {
		require.Nil(t, flags.Lookup(name), name)
	}
	require.NotNil(t, flags.Lookup("vmodule"))
	require.NoError(t, flags.Parse([]string{"-v=4"}))
	require.True(t, klog.V(4).Enabled())
	require.False(t, klog.V(5).Enabled())
}

func TestConfigureWritesEverySeverityOnceAndMirrorsOnlyWhenRequested(t *testing.T) {
	for _, mirror := range []bool{false, true} {
		t.Run(fmt.Sprintf("mirror=%t", mirror), func(t *testing.T) {
			state := klog.CaptureState()
			defer state.Restore()
			directory := t.TempDir()
			path := writeConfig(t, fmt.Sprintf("directory: %q\nalsoStderr: %t\n", directory, mirror))
			t.Setenv(ConfigEnv, path)
			stderr := captureStderr(t)
			closeLogs, err := Configure("fastlet")
			require.NoError(t, err)
			defer closeLogs()
			klog.InfoS("info marker", "sandboxName", "sandbox-a")
			klog.Warning("warning marker")
			klog.ErrorS(errors.New("test failure"), "error marker")
			ctx := klog.NewContext(context.Background(), klog.Background().WithValues("request_id", "request-a"))
			klog.FromContext(ctx).Info("context marker")
			closeLogs()

			data, err := os.ReadFile(filepath.Join(directory, "fastlet.log"))
			require.NoError(t, err)
			for _, marker := range []string{"info marker", "warning marker", "error marker", "context marker"} {
				require.Equal(t, 1, strings.Count(string(data), marker), marker)
			}
			require.Contains(t, string(data), "sandboxName=\"sandbox-a\"")
			require.Contains(t, string(data), "request_id=\"request-a\"")
			mirrored, err := os.ReadFile(stderr.Name())
			require.NoError(t, err)
			if mirror {
				require.Equal(t, data, mirrored)
			} else {
				require.Empty(t, mirrored)
			}
			info, err := os.Stat(filepath.Join(directory, "fastlet.log"))
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		})
	}
}

func TestConfigureFailsBeforeServingWhenConfigOrOutputIsInvalid(t *testing.T) {
	t.Setenv(ConfigEnv, filepath.Join(t.TempDir(), "missing.yaml"))
	_, err := Configure("fastlet")
	require.ErrorIs(t, err, os.ErrNotExist)

	directory := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(directory, "fastlet.log"), 0o750))
	t.Setenv(ConfigEnv, writeConfig(t, fmt.Sprintf("directory: %q\n", directory)))
	_, err = Configure("fastlet")
	require.ErrorContains(t, err, "open component log")

	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	t.Setenv(ConfigEnv, writeConfig(t, fmt.Sprintf("directory: %q\n", file)))
	_, err = Configure("fastlet")
	require.ErrorContains(t, err, "create logging directory")
}

func TestRotationPreservesHistoryCompressesAndLimitsBackups(t *testing.T) {
	directory := t.TempDir()
	filename := filepath.Join(directory, "fastlet.log")
	oldContent := []byte("old marker\n" + strings.Repeat("x", 900*1024))
	require.NoError(t, os.WriteFile(filename, oldContent, 0o600))
	for index := 1; index <= 3; index++ {
		stamp := time.Now().UTC().Add(-time.Duration(index) * time.Hour).Format("2006-01-02T15-04-05.000")
		require.NoError(t, os.WriteFile(filepath.Join(directory, "fastlet-"+stamp+".log"), []byte("earlier backup"), 0o600))
	}
	logger, err := newLogger(Config{Directory: directory, MaxSizeMB: 1, MaxBackups: 1, Compress: true}, "fastlet")
	require.NoError(t, err)
	defer func() { require.NoError(t, logger.Close()) }()
	newContent := []byte("new marker\n" + strings.Repeat("y", 200*1024))
	_, err = logger.Write(newContent)
	require.NoError(t, err)
	active, err := os.ReadFile(filename)
	require.NoError(t, err)
	require.Equal(t, newContent, active)
	require.Eventually(t, func() bool {
		files, globErr := filepath.Glob(filepath.Join(directory, "fastlet-*.log*"))
		if globErr != nil || len(files) != 1 || !strings.HasSuffix(files[0], ".gz") {
			return false
		}
		compressed, readErr := os.ReadFile(files[0])
		if readErr != nil {
			return false
		}
		reader, gzipErr := gzip.NewReader(bytes.NewReader(compressed))
		if gzipErr != nil {
			return false
		}
		defer reader.Close()
		contents, readErr := io.ReadAll(reader)
		return readErr == nil && bytes.Equal(contents, oldContent)
	}, 5*time.Second, 10*time.Millisecond)
}

func TestBackupAgeAndUnlimitedRetention(t *testing.T) {
	for _, maxAge := range []int{0, 1} {
		t.Run(fmt.Sprintf("maxAge=%d", maxAge), func(t *testing.T) {
			directory := t.TempDir()
			old := filepath.Join(directory, "janitor-2000-01-01T00-00-00.000.log")
			recent := filepath.Join(directory, "janitor-"+time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15-04-05.000")+".log")
			require.NoError(t, os.WriteFile(old, []byte("old"), 0o600))
			require.NoError(t, os.WriteFile(recent, []byte("recent"), 0o600))
			logger, err := newLogger(Config{Directory: directory, MaxSizeMB: 1, MaxAgeDays: maxAge}, "janitor")
			require.NoError(t, err)
			defer func() { require.NoError(t, logger.Close()) }()
			if maxAge > 0 {
				require.Eventually(t, func() bool {
					_, err := os.Stat(old)
					return errors.Is(err, os.ErrNotExist)
				}, 5*time.Second, 10*time.Millisecond)
			} else {
				require.FileExists(t, old)
			}
			require.FileExists(t, recent)
		})
	}
}

func TestReopenAppendsToExistingLog(t *testing.T) {
	config := Config{Directory: t.TempDir(), MaxSizeMB: 1, MaxBackups: 2}
	for _, message := range []string{"before restart\n", "after restart\n"} {
		logger, err := newLogger(config, "fastlet")
		require.NoError(t, err)
		_, err = logger.Write([]byte(message))
		require.NoError(t, err)
		require.NoError(t, logger.Close())
	}
	data, err := os.ReadFile(filepath.Join(config.Directory, "fastlet.log"))
	require.NoError(t, err)
	require.Equal(t, "before restart\nafter restart\n", string(data))
}

func TestConcurrentLoggingAndComponentIsolation(t *testing.T) {
	state := klog.CaptureState()
	defer state.Restore()
	directory := t.TempDir()
	t.Setenv(ConfigEnv, writeConfig(t, fmt.Sprintf("directory: %q\n", directory)))
	closeLogs, err := Configure("fastlet")
	require.NoError(t, err)
	defer closeLogs()
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for entry := range 100 {
				klog.InfoS("concurrent marker", "worker", worker, "entry", entry)
			}
		}()
	}
	workers.Wait()
	closeLogs()
	closeProxyLogs, err := Configure("fastlet-proxy")
	require.NoError(t, err)
	defer closeProxyLogs()
	klog.InfoS("proxy marker")
	closeProxyLogs()
	data, err := os.ReadFile(filepath.Join(directory, "fastlet.log"))
	require.NoError(t, err)
	require.Equal(t, 800, strings.Count(string(data), "concurrent marker"))
	require.NotContains(t, string(data), "proxy marker")
	proxyData, err := os.ReadFile(filepath.Join(directory, "fastlet-proxy.log"))
	require.NoError(t, err)
	require.Contains(t, string(proxyData), "proxy marker")
}

func TestWriteFailuresAreReportedOutsideKlog(t *testing.T) {
	var diagnostics bytes.Buffer
	writer := reportErrors{output: failingWriter{}, diagnostics: &diagnostics}
	_, err := writer.Write([]byte("message"))
	require.ErrorContains(t, err, "disk full")
	require.Contains(t, diagnostics.String(), "write component log: disk full")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	return path
}

func captureStderr(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr-")
	require.NoError(t, err)
	previous := os.Stderr
	os.Stderr = file
	t.Cleanup(func() {
		os.Stderr = previous
		require.NoError(t, file.Close())
	})
	return file
}
