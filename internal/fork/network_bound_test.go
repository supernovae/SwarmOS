package fork

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/testutil/gittransport"
	"testing"
	"time"
)

func TestForkCoordinatorNetworkBounded(t *testing.T) {
	repo := gitfixture.Init(t, "")
	remote := gitfixture.Init(t, "", "--bare")
	gitfixture.Run(t, repo, "remote", "add", "origin", remote)
	tr := gittransport.New(t, remote)
	tr.Configure(t, repo, "origin")
	tr.Set("upload", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	tr.Watch(15*time.Second, func() { _, err = runGitWithToken(ctx, repo, "ls-remote", "origin") })
	if !errors.Is(err, gitcredential.ErrInterrupted) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("query: %v", err)
	}
}
