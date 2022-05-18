// package daemonclient is a wrapper around a grpc client
// to talk to turbod

package daemonclient

import (
	"context"

	"github.com/vercel/turborepo/cli/internal/server"
)

type DaemonClient struct {
	client server.TurboClient
	ctx    context.Context
}

func New(ctx context.Context, client server.TurboClient) *DaemonClient {
	return &DaemonClient{
		client: client,
		ctx:    ctx,
	}
}

func (d *DaemonClient) ShouldRestore(hash string, repoRelativeOutputGlobs []string) ([]string, error) {
	reply, err := d.client.ShouldRestoreDirectory(d.ctx, &server.ShouldRestoreDirectoryRequest{})
	if err != nil {
		return nil, err
	}
	return reply.OkToRestore, nil
}

func (d *DaemonClient) MarkSaved(hash string, repoRelativeOutputGlobs []string) error {
	_, err := d.client.NotifyDirectoriesBuilt(d.ctx, &server.NotifyDirectioresBuiltRequest{
		Directory: repoRelativeOutputGlobs,
	})
	return err
}
