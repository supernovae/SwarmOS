package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type syntheticEntry struct {
	os.DirEntry
	name string
}

func (e syntheticEntry) Name() string { return e.name }
func TestSyntheticDefaultEntryBoundAndCancellation(t *testing.T) {
	for _, count := range []int{99999, 100000, 100001} {
		attempts, visits := 0, 0
		s := scanner{ctx: context.Background(), b: Budget{Limits: DefaultLimits()}, out: Snapshot{Complete: true}}
		s.opts.ReadDirectory = func(ctx context.Context, path string, visit func(os.DirEntry) error) error {
			for i := 0; i < count; i++ {
				attempts++
				if err := visit(syntheticEntry{name: "x"}); err != nil {
					return err
				}
			}
			return nil
		}
		s.directory("/synthetic", func(os.DirEntry) { visits++ })
		if visits != min(count, 100000) || attempts != min(count, 100001) || s.b.Counters.Entries > 100000 || s.out.Complete != (count <= 100000) {
			t.Fatal(count, visits, attempts, s.out)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := scanner{ctx: ctx, b: Budget{Limits: DefaultLimits()}, out: Snapshot{Complete: true}}
	attempts := 0
	s.opts.ReadDirectory = func(ctx context.Context, path string, visit func(os.DirEntry) error) error {
		for {
			attempts++
			if err := visit(syntheticEntry{name: "x"}); err != nil {
				return err
			}
		}
	}
	s.directory("/synthetic", func(os.DirEntry) {
		if s.b.Counters.Entries == 128 {
			cancel()
		}
	})
	if attempts != 128 || s.b.Counters.Entries != 128 || s.out.Complete {
		t.Fatal(attempts, s.b.Counters)
	}
}
func TestSyntheticDefaultResourceBound(t *testing.T) {
	s := scanner{ctx: context.Background(), b: Budget{Limits: DefaultLimits()}, out: Snapshot{Complete: true}, seen: map[string]bool{}}
	for i := 0; i < 10001; i++ {
		s.add(Resource{Owner: "local", Class: "code-branch", Path: "/r", Ref: "r", Identity: strconv.Itoa(i)})
	}
	if len(s.out.Resources) != 10000 || s.b.Counters.Records != 10000 || s.out.Complete {
		t.Fatal(len(s.out.Resources), s.b.Counters)
	}
	if s.b.Counters.Bytes > s.b.Limits.Bytes {
		t.Fatal("over-budget admission")
	}
}
func TestSyntheticWorkingSetLimit(t *testing.T) {
	s := scanner{ctx: context.Background(), b: Budget{Limits: DefaultLimits()}, out: Snapshot{Complete: true}, seen: map[string]bool{}}
	// Long but individually bounded records exhaust the memory budget before
	// the record cap. No filesystem or large physical fixture is involved.
	text := fmt.Sprintf("%08192d", 1)
	for i := 0; i < 1000; i++ {
		s.add(Resource{Owner: "local", Class: "code-branch", Path: text, Ref: "r", Identity: strconv.Itoa(i)})
	}
	if s.out.Complete || s.b.Counters.Bytes > 64<<20 || len(s.out.Resources) >= 1000 {
		t.Fatal(s.b.Counters, len(s.out.Resources))
	}
}

func TestSyntheticDefaultRepositoryBound(t *testing.T) {
	root := t.TempDir()
	calls := 0
	s := scanner{ctx: context.Background(), b: Budget{Limits: DefaultLimits()}, out: Snapshot{Complete: true}, seen: map[string]bool{}, repos: map[string]string{}, inodes: map[fileIdentity]bool{}}
	s.opts.GitOutput = func(ctx context.Context, path string, args ...string) (io.ReadCloser, error) {
		calls++
		value := ""
		if args[0] == "rev-parse" {
			value = filepath.Join(path, ".git")
		}
		return io.NopCloser(strings.NewReader(value)), nil
	}
	for i := 0; i < 257; i++ {
		path := filepath.Join(root, strconv.Itoa(i))
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		s.repository("local/repo"+strconv.Itoa(i), path, "")
	}
	if s.b.Counters.Repositories != 256 || len(s.repos) != 256 || calls > 3*256+1 {
		t.Fatal(s.b.Counters, len(s.repos), calls)
	}
}

type endlessMetadata struct{ bytes int }

func (r *endlessMetadata) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.bytes += len(p)
	return len(p), nil
}
func (r *endlessMetadata) Close() error { return nil }
func TestSyntheticGitOutputStopsAtCap(t *testing.T) {
	source := &endlessMetadata{}
	s := scanner{ctx: context.Background(), b: Budget{Limits: DefaultLimits()}}
	s.opts.GitOutput = func(context.Context, string, ...string) (io.ReadCloser, error) { return source, nil }
	if _, err := s.git("/fixture", "rev-parse", "--show-toplevel"); err == nil {
		t.Fatal("unbounded Git output accepted")
	}
	if source.bytes > (128<<10)+32768 {
		t.Fatal("read beyond bounded lookahead", source.bytes)
	}
}

func TestUsageExhaustionPreservesDiscovery(t *testing.T) {
	opts := fixture(t)
	path := filepath.Join(opts.CacheRoot, "agent-build", "orphan", "go")
	for i := 0; i < 20; i++ {
		put(t, filepath.Join(path, fmt.Sprint(i)), "data")
	}
	s := scanner{ctx: context.Background(), b: Budget{Limits: DefaultLimits()}, out: Snapshot{Complete: true}, inodes: map[fileIdentity]bool{}, physical: true}
	s.b.Limits.Entries = 2
	before := s.b.Counters
	r := Resource{Path: path}
	s.account(&r)
	if !s.out.Complete || len(s.out.Reasons) != 0 || s.b.Counters.Entries != before.Entries || s.b.Counters.Bytes != before.Bytes || r.LogicalBytes != nil || r.UsageReason == "" || s.physical {
		t.Fatalf("scanner %+v resource %+v", s.out, r)
	}
}
