package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// Read a line without prefetching the next turn or leaving a blocked reader
// behind on cancellation. Live CLI input is a file (terminal or pipe).
func nativeInstruction(ctx context.Context, input io.Reader) (string, error) {
	if budget, ok := ctx.Value(agentBudgetKey{}).(*agentBudget); ok {
		budget.pause()
		defer budget.resume()
	}
	var line strings.Builder
	for line.Len() < 64<<10 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var b [1]byte
		var n int
		var err error
		switch reader := input.(type) {
		case nil:
			return line.String(), io.EOF
		case *os.File:
			n, err = nativeInputByte(ctx, reader, b[:])
		case *strings.Reader, *bytes.Reader, *bytes.Buffer:
			n, err = reader.Read(b[:])
		default:
			return "", fmt.Errorf("native interactive input requires a terminal, file, pipe, or buffered text")
		}
		if n > 0 {
			if b[0] == '\n' {
				return line.String(), nil
			}
			line.WriteByte(b[0])
		}
		if err != nil {
			return line.String(), err
		}
	}
	return "", fmt.Errorf("follow-up instruction exceeds 64 KiB")
}
