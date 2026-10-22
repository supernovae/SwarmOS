package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// ServeTools exposes task tools over stdio, with cancellation and bounded
// concurrency. It has no credential, resource or orchestration operations.
func ServeTools(ctx context.Context, e ToolExecutor, input io.Reader, output io.Writer) error {
	ctx, stop := context.WithTimeout(ctx, 15*time.Minute)
	var writes sync.Mutex
	write := func(value any) error {
		writes.Lock()
		defer writes.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return json.NewEncoder(output).Encode(value)
	}
	var calls sync.WaitGroup
	var pendingMu sync.Mutex
	pending := map[string]context.CancelFunc{}
	defer func() {
		stop()
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
		if closer, ok := output.(io.Closer); ok {
			_ = closer.Close()
		}
		calls.Wait()
	}()
	frames := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 256<<10)
		for scanner.Scan() {
			frame := append([]byte(nil), scanner.Bytes()...)
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
		readErr <- scanner.Err()
	}()
	failures := make(chan error, 8)
	for {
		var frame []byte
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case err := <-failures:
			return err
		case frame = <-frames:
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &request); err != nil {
			return fmt.Errorf("invalid MCP frame: %w", err)
		}
		if request.JSONRPC != "2.0" {
			return fmt.Errorf("unsupported MCP transport")
		}
		if len(request.ID) == 0 {
			if request.Method == "notifications/cancelled" {
				var cancel struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(request.Params, &cancel) == nil {
					pendingMu.Lock()
					if fn := pending[string(cancel.RequestID)]; fn != nil {
						fn()
					}
					pendingMu.Unlock()
				}
			}
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "swarm-tools", "version": "1"}}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result = map[string]any{"tools": executorTools()}
		case "tools/call":
			var call struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(request.Params, &call); err != nil {
				return err
			}
			pendingMu.Lock()
			if _, exists := pending[string(request.ID)]; exists || len(pending) >= 8 {
				pendingMu.Unlock()
				return fmt.Errorf("MCP tool concurrency limit or duplicate request ID")
			}
			callCtx, cancel := context.WithCancel(ctx)
			pending[string(request.ID)] = cancel
			pendingMu.Unlock()
			calls.Add(1)
			go func(id json.RawMessage, name string, args json.RawMessage) {
				defer calls.Done()
				defer cancel()
				result := e.Call(callCtx, name, args)
				if err := write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
					select {
					case failures <- err:
					default:
					}
				}
				pendingMu.Lock()
				delete(pending, string(id))
				pendingMu.Unlock()
			}(request.ID, call.Name, call.Arguments)
			continue
		default:
			if err := write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "unsupported method"}}); err != nil {
				return err
			}
			continue
		}
		if err := write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			return err
		}
	}
}
