package server

import (
	"context"

	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
	"github.com/vercel/turborepo/cli/internal/filewatcher"
	"github.com/vercel/turborepo/cli/internal/fs"
	"github.com/vercel/turborepo/cli/internal/globwatcher"
	"google.golang.org/grpc"
)

// Server implements the GRPC serverside of TurboServer
// Note for the future: we don't yet make use of turbo.json
// or the package graph in the server. Once we do, we may need a
// layer of indirection between "the thing that responds to grpc requests"
// and "the thing that holds our persistent data structures" to handle
// changes in the underlying configuration.
type Server struct {
	UnimplementedTurboServer
	watcher     *filewatcher.FileWatcher
	globWatcher *globwatcher.GlobWatcher
}

// New returns a new instance of Server
func New(logger hclog.Logger, repoRoot fs.AbsolutePath) (*Server, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fileWatcher := filewatcher.New(logger.Named("FileWatcher"), repoRoot, watcher)
	globWatcher := globwatcher.New(logger.Named("GlobWatcher"), repoRoot)
	server := &Server{
		watcher:     fileWatcher,
		globWatcher: globWatcher,
	}
	server.watcher.AddClient(globWatcher)
	if err := server.watcher.Start(); err != nil {
		return nil, errors.Wrapf(err, "watching %v", repoRoot)
	}
	return server, nil
}

// Close is used for shutting down this copy of the server
func (s *Server) Close() error {
	return s.watcher.Close()
}

// Register registers this server to respond to GRPC requests
func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	RegisterTurboServer(registrar, s)
}

// NotifyOutputsWritten implements the NotifyOutputsWritten rpc from turbo.proto
func (s *Server) NotifyOutputsWritten(ctx context.Context, req *NotifyOutputsWrittenRequest) (*NotifyOutputsWrittenResponse, error) {
	err := s.globWatcher.WatchGlobs(req.Hash, req.OutputGlobs)
	if err != nil {
		return nil, err
	}
	return &NotifyOutputsWrittenResponse{}, nil
}

// GetChangedOutputs implements the GetChangedOutputs rpc from turbo.proto
func (s *Server) GetChangedOutputs(ctx context.Context, req *GetChangedOutputsRequest) (*GetChangedOutputsResponse, error) {
	changedGlobs, err := s.globWatcher.GetChangedGlobs(req.Hash, req.OutputGlobs)
	if err != nil {
		return nil, err
	}
	return &GetChangedOutputsResponse{
		ChangedOutputGlobs: changedGlobs,
	}, nil
}
