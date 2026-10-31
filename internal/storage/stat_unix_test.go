//go:build linux || darwin

package storage

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type syntheticInfo struct {
	device, inode uint64
	blocks        int64
}

func (s syntheticInfo) Name() string       { return "synthetic" }
func (s syntheticInfo) Size() int64        { return 1 << 20 }
func (s syntheticInfo) Mode() os.FileMode  { return 0600 }
func (s syntheticInfo) ModTime() time.Time { return time.Time{} }
func (s syntheticInfo) IsDir() bool        { return false }
func (s syntheticInfo) Sys() any {
	stat := &syscall.Stat_t{Dev: 1, Ino: s.inode, Blocks: s.blocks}
	if s.device == 2 {
		stat.Dev = 2
	}
	return stat
}

// The adapter uses a real stat representation, with device identity exercised
// separately below because Darwin and Linux use different Dev integer types.
func TestSyntheticDefaultIdentityLimitAndDeviceSeparation(t *testing.T) {
	s := scanner{ctx: context.Background(), b: Budget{Limits: DefaultLimits()}, out: Snapshot{Complete: true}, physical: true, inodes: map[fileIdentity]bool{}}
	for i := 0; i < 100000; i++ {
		var local int64
		if !s.allocate(syntheticInfo{inode: uint64(i), blocks: 8}, &local) {
			t.Fatal(i)
		}
		if local != 4096 {
			t.Fatal(local)
		}
	}
	var local int64
	s.allocate(syntheticInfo{inode: 100000, blocks: 8}, &local)
	if s.physical || s.out.Complete || s.b.Counters.Identities != 100000 {
		t.Fatal(s.b.Counters)
	}
	// Equal inode numbers on different devices must remain separate observations.
	other := scanner{b: Budget{Limits: DefaultLimits()}, out: Snapshot{Complete: true}, physical: true, inodes: map[fileIdentity]bool{}}
	for _, device := range []uint64{1, 2} {
		var amount int64
		other.allocate(syntheticInfo{device: device, inode: 7, blocks: 8}, &amount)
	}
	if other.b.Counters.Identities != 2 || other.out.ObservedAllocatedBytes != 8192 {
		t.Fatal(other.b.Counters, other.out.ObservedAllocatedBytes)
	}

}
func TestSparseAndExternalHardlinkObservations(t *testing.T) {
	opts := fixture(t)
	path := filepath.Join(opts.CacheRoot, "agent-build", "orphan", "go", "sparse")
	put(t, path, "")
	file, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(8 << 20); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	outside := filepath.Join(t.TempDir(), "outside")
	if err = os.Link(path, outside); err != nil {
		t.Skip(err)
	}
	out, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Resources) != 1 || out.Resources[0].LogicalBytes == nil || *out.Resources[0].LogicalBytes != 8<<20 {
		t.Fatal(out)
	}
	r := out.Resources[0]
	if r.AllocatedBytes == nil || *r.AllocatedBytes >= *r.LogicalBytes {
		t.Skip("filesystem does not expose sparse allocation")
	}
	if r.Eligibility != "unknown" {
		t.Fatal("external link or sparse layout granted eligibility")
	}
}
