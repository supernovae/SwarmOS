package process

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestTailKeepsLastBytes(t *testing.T) {
	tests := []struct {
		name      string
		writes    []string
		want      string
		truncated bool
	}{
		{"empty", nil, "", false},
		{"fits", []string{"ab", "cd"}, "abcd", false},
		{"exact", []string{"abcdefgh"}, "abcdefgh", false},
		{"split overflow", []string{"abcde", "fghij"}, "cdefghij", true},
		{"single write larger than capacity", []string{"0123456789abc"}, "56789abc", true},
		{"large write after content", []string{"x", "12345678"}, "12345678", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tail := NewTail(8)
			for _, write := range test.writes {
				if n, err := tail.Write([]byte(write)); n != len(write) || err != nil {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			got, truncated := tail.Bytes()
			if string(got) != test.want || truncated != test.truncated {
				t.Fatalf("Bytes = %q, %v; want %q, %v", got, truncated, test.want, test.truncated)
			}
		})
	}
}

func TestTailConcurrentWritesStayBounded(t *testing.T) {
	tail := NewTail(1024)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chunk := bytes.Repeat([]byte{byte('a' + i)}, 300)
			for range 100 {
				_, _ = tail.Write(chunk)
			}
		}()
	}
	wg.Wait()
	got, truncated := tail.Bytes()
	if len(got) != 1024 || !truncated || cap(got) > 4096 {
		t.Fatalf("len=%d truncated=%v", len(got), truncated)
	}
	if strings.Trim(string(got), "abcdefgh") != "" {
		t.Fatal("tail contains bytes that were never written")
	}
}
