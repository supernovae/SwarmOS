package sandbox

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// runFollowupTurns is the coordinator-owned input loop shared by native Codex
// and Claude. A delivered callback never observes output or rejected input.
func runFollowupTurns(ctx context.Context, prompt, prefix string, interactive, trim bool, input io.Reader, stdout io.Writer, execute func(string) error, delivered func(string)) error {
	for {
		if err := execute(prompt); err != nil {
			return err
		}
		if !interactive {
			return nil
		}
		fmt.Fprint(stdout, "\nFollow-up instruction (Enter to finish this phase): ")
		line, err := nativeInstruction(ctx, input)
		if err != nil && err != io.EOF {
			return err
		}
		if strings.TrimSpace(line) == "" {
			return nil
		}
		if trim {
			line = strings.TrimSpace(line)
		}
		if delivered != nil {
			delivered(line)
		}
		prompt = prefix + line
	}
}
