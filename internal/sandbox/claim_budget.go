package sandbox

import (
	"context"
	"sort"
)

func checkImport(ctx context.Context, opts WorkerOptions, initial, final workerManifest) error {
	var changed, deleted []string
	for name, before := range initial {
		after, ok := final[name]
		if !ok {
			deleted = append(deleted, name)
		}
		if !ok || before != after {
			changed = append(changed, name)
		}
	}
	for name := range final {
		if _, ok := initial[name]; !ok {
			changed = append(changed, name)
		}
	}
	sort.Strings(deleted)
	if opts.CheckImport != nil {
		return opts.CheckImport(ctx, changed, deleted)
	}
	return ctx.Err()
}
