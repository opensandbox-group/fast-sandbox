package agent

import "os"

// The node cache commits a set with renames: every artifact is renamed into
// place, then manifest.json, the commit point. A rename is atomic but not
// durable on its own: after a power loss the filesystem may persist the
// renamed names before the data blocks behind them, leaving a committed
// manifest that points at zero-length or torn files. Every cache commit
// therefore fsyncs each file before its rename and the directory after the
// renames. fsyncFile and fsyncDir are variables so tests can observe what a
// commit makes durable.
var (
	fsyncFile = func(file *os.File) error { return file.Sync() }
	fsyncDir  = syncDirectory
)

// syncDirectory fsyncs a directory so the renames into it are durable.
func syncDirectory(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// SyncCacheFile fsyncs the data and metadata of a file that is about to be
// renamed into the node cache. Callers that commit cache entries outside the
// pull path (the driver's same-node snapshot cache) use it so their commits
// are as durable as a pull.
func SyncCacheFile(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	return fsyncFile(handle)
}

// SyncCacheDir fsyncs a cache directory after renames into it.
func SyncCacheDir(dir string) error {
	return fsyncDir(dir)
}
