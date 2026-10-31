package storage

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
)

func (s *scanner) account(r *Resource) {
	// Usage is advisory and has a separate per-resource allowance. It cannot
	// consume ownership discovery's entries or retained-memory allowance.
	accounting := *s
	accounting.b = Budget{Limits: s.b.Limits}
	accounting.out = Snapshot{Complete: true}
	accounting.accountUsage(r)
	s.physical = accounting.physical && r.LogicalBytes != nil
	s.b.Counters.Identities = len(s.inodes)
	s.out.ObservedAllocatedBytes += accounting.out.ObservedAllocatedBytes
}

func (s *scanner) accountUsage(r *Resource) {
	root, err := openExisting(filepath.Dir(r.Path))
	if err != nil {
		r.UsageReason = "path absent or unsafe"
		s.fail("usage", err)
		return
	}
	defer root.Close()
	var logical, allocated int64
	complete := true
	// Depth is bounded before opening the next directory, keeping descriptors
	// and recursive frames bounded even on adversarially deep trees.
	var walk func(string, int)
	walk = func(name string, depth int) {
		if !complete {
			return
		}
		if err := s.b.entry(s.ctx); err != nil {
			complete = false
			s.fail("usage", err)
			return
		}
		if depth > 64 {
			complete = false
			s.fail("usage depth", ErrLimit)
			return
		}
		// OpenDirectory and OpenRegular verify parent components and file type.
		dir, err := root.OpenDirectory(name)
		if err == nil {
			defer dir.Close()
			info, e := dir.Stat()
			if e != nil {
				complete = false
				return
			}
			if !s.allocate(info, &allocated) {
				complete = false
				return
			}
			for {
				if err := s.ctx.Err(); err != nil {
					complete = false
					s.fail("usage", err)
					return
				}
				entries, e := dir.ReadDir(1)
				if len(entries) == 0 {
					if e != nil && !errors.Is(e, io.EOF) {
						complete = false
					}
					break
				}
				child := filepath.Join(name, entries[0].Name())
				if e := s.b.Charge(int64(256 + 4*len(child))); e != nil {
					complete = false
					s.fail("usage", e)
					return
				}
				walk(child, depth+1)
				if !complete {
					return
				}
			}
			after, e := dir.Stat()
			if e != nil || !info.ModTime().Equal(after.ModTime()) {
				complete = false
			}
			return
		}
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || logical > math.MaxInt64-info.Size() {
			complete = false
			return
		}
		logical += info.Size()
		if !s.allocate(info, &allocated) {
			complete = false
		}

	}
	walk(filepath.Base(r.Path), 0)
	if !complete {
		r.UsageReason = "usage incomplete: missing, changing, unsafe, unsupported or bounded input"
		s.fail("usage", errors.New("incomplete"))
		return
	}
	r.LogicalBytes = &logical
	if s.physical {
		r.AllocatedBytes = &allocated
	} else {
		r.UsageReason = "allocated usage unknown: identity coverage incomplete"
	}
}
func (s *scanner) allocate(info os.FileInfo, local *int64) bool {
	key, blocks, ok := physicalStat(info)
	if !ok {
		s.physical = false
		return true
	}
	if *local > math.MaxInt64-blocks {
		return false
	}
	*local += blocks
	if !s.physical || s.inodes[key] {
		return true
	}
	if len(s.inodes) >= s.b.Limits.Identities {
		s.physical = false
		s.fail("inode coverage", ErrLimit)
		return true
	}
	if err := s.b.Charge(128); err != nil {
		s.physical = false
		s.fail("inode coverage", err)
		return true
	}
	if s.out.ObservedAllocatedBytes > math.MaxInt64-blocks {
		s.physical = false
		return false
	}
	s.b.Counters.Identities++
	s.inodes[key] = true
	s.out.ObservedAllocatedBytes += blocks
	return true
}
