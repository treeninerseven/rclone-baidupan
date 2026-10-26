package baidupan

import (
	"fmt"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest/fstests"
)

// SetUploadChunkSize adjusts chunk sizes for the standard integration tests.
func (f *Fs) SetUploadChunkSize(size fs.SizeSuffix) (fs.SizeSuffix, error) {
	old := f.opt.ChunkSize
	if size != 4*fs.Mebi && size != 16*fs.Mebi && size != 32*fs.Mebi {
		return old, fmt.Errorf("unsupported chunk size %v", size)
	}
	f.opt.ChunkSize = size
	return old, nil
}

// TestIntegration runs the standard suite against a separately configured test remote.
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestBaidupan:",
		NilObject:  (*Object)(nil),
		ChunkedUpload: fstests.ChunkedUploadConfig{
			MinChunkSize: 4 * fs.Mebi,
			MaxChunkSize: 4 * fs.Mebi,
		},
	})
}
