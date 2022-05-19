package server

import (
	context "context"
	"errors"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/go-hclog"
	"github.com/vercel/turborepo/cli/globwatcher"
	"github.com/vercel/turborepo/cli/internal/fs"
	"google.golang.org/grpc"
)

type Server struct {
	UnimplementedTurboServer
	watcher     fileWatcher
	globWatcher *globwatcher.GlobWatcher
}

type fileWatcher struct {
	*fsnotify.Watcher

	logger hclog.Logger

	clientsMu sync.RWMutex
	clients   []FileWatchClient
	closed    bool
}

func (fw *fileWatcher) watch() {
outer:
	for {
		select {
		case ev, ok := <-fw.Watcher.Events:
			if !ok {
				fw.logger.Info("Events channel closed. Exiting watch loop")
				break outer
			}
			fw.clientsMu.RLock()
			for _, client := range fw.clients {
				client.OnFileWatchEvent(ev)
			}
			fw.clientsMu.RUnlock()
		case err, ok := <-fw.Watcher.Errors:
			if !ok {
				fw.logger.Info("Errors channel closed. Exiting watch loop")
				break outer
			}
			fw.clientsMu.RLock()
			for _, client := range fw.clients {
				client.OnFileWatchError(err)
			}
			fw.clientsMu.RUnlock()
		}
	}
	fw.clientsMu.Lock()
	fw.closed = true
	for _, client := range fw.clients {
		client.OnFileWatchingClosed()
	}
	fw.clientsMu.Unlock()
}

func (fw *fileWatcher) AddClient(client FileWatchClient) {
	fw.clientsMu.Lock()
	defer fw.clientsMu.Unlock()
	fw.clients = append(fw.clients, client)
	if fw.closed {
		client.OnFileWatchingClosed()
	}
}

// FileWatchClient defines the callbacks used by the file watching loop.
// All methods are called from the same goroutine so they:
// 1) do not need synchronization
// 2) should minimize the work they are doing when called, if possible
type FileWatchClient interface {
	OnFileWatchEvent(ev fsnotify.Event)
	OnFileWatchError(err error)
	OnFileWatchingClosed()
}

func New(logger hclog.Logger, repoRoot fs.AbsolutePath) (*Server, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	globWatcher := globwatcher.New(logger.Named("GlobWatcher"), repoRoot)
	server := &Server{
		watcher: fileWatcher{
			Watcher: watcher,
			logger:  logger.Named("FileWatcher"),
		},
		globWatcher: globWatcher,
	}
	server.watcher.AddClient(globWatcher)
	go server.watcher.watch()
	return server, nil
}

func (s *Server) Close() error {
	return s.watcher.Close()
}

func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	RegisterTurboServer(registrar, s)
}

func (s *Server) Ping(ctx context.Context, req *PingRequest) (*PingReply, error) {
	return &PingReply{}, nil
}

func (s *Server) NotifyOutputsWritten(ctx context.Context, req *NotifyOutputsWrittenRequest) (*NotifyOutputsWrittenResponse, error) {
	err := s.globWatcher.WatchGlobs(&s.watcher, req.Hash, req.OutputGlobs)
	if err != nil {
		return nil, err
	}
	return &NotifyOutputsWrittenResponse{}, nil
}

func (s *Server) GetChangedOutputs(ctx context.Context, req *GetChangedOutputsRequest) (*GetChangedOutputsResponse, error) {
	changedGlobs, err := s.globWatcher.GetChangedGlobs(req.Hash)
	if errors.Is(err, globwatcher.ErrNotTracked) {
		// If we're not tracking it, mark everything as changed
		changedGlobs = req.OutputGlobs
	} else if err != nil {
		return nil, err
	}
	return &GetChangedOutputsResponse{
		ChangedOutputGlobs: changedGlobs,
	}, nil
}
