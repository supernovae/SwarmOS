package sandbox

import "context"

func checkImport(ctx context.Context, opts WorkerOptions, initial, final workerManifest) error {
	var changed []string
	for name, before := range initial {
		if after, ok := final[name]; !ok || before != after {
			changed = append(changed, name)
		}
	}
	for name := range final {
		if _, ok := initial[name]; !ok {
			changed = append(changed, name)
		}
	}
	if opts.CheckImport != nil {
		return opts.CheckImport(ctx, changed)
	}
	return ctx.Err()
}
