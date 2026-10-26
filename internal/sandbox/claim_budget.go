package sandbox

import "context"

func checkPauseImport(ctx context.Context, opts WorkerOptions, initial, final workerManifest) error {
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
	if opts.CheckPauseImport != nil {
		return opts.CheckPauseImport(ctx, changed)
	}
	return ctx.Err()
}
