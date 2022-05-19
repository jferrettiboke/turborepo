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

func (d *DaemonClient) GetChangedOutputs(hash string, repoRelativeOutputGlobs []string) ([]string, error) {
	reply, err := d.client.GetChangedOutputs(d.ctx, &server.GetChangedOutputsRequest{
		Hash:        hash,
		OutputGlobs: repoRelativeOutputGlobs,
	})
	if err != nil {
		return nil, err
	}
	return reply.ChangedOutputGlobs, nil
}

func (d *DaemonClient) NotifyOutputsWritten(hash string, repoRelativeOutputGlobs []string) error {
	_, err := d.client.NotifyOutputsWritten(d.ctx, &server.NotifyOutputsWrittenRequest{
		Hash:        hash,
		OutputGlobs: repoRelativeOutputGlobs,
	})
	return err
}
