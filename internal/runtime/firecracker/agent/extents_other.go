//go:build !linux

package agent

import "os"

// dataChunkOffsets cannot consult the extent map off Linux: every chunk is
// read and zero-checked.
func dataChunkOffsets(*os.File, int64, int64) ([]int64, bool) { return nil, false }
