package server

import (
	context "context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/go-hclog"
	"github.com/karrick/godirwalk"
	"github.com/pkg/errors"
	"github.com/vercel/turborepo/cli/globwatcher"
	"github.com/vercel/turborepo/cli/internal/fs"
	"google.golang.org/grpc"
)

type Server struct {
	UnimplementedTurboServer
	watcher     *fileWatcher
	globWatcher *globwatcher.GlobWatcher
}

type fileWatcher struct {
	*fsnotify.Watcher

	logger hclog.Logger

	clientsMu sync.RWMutex
	clients   []FileWatchClient
	closed    bool
}

var _ignores = []string{".git", "node_modules"}

func (fw *fileWatcher) watchRecursively(root fs.AbsolutePath) error {
	excludes := make([]string, len(_ignores))
	for i, ignore := range _ignores {
		excludes[i] = filepath.FromSlash(root.Join(ignore).ToString())
	}
	excludePattern := "{" + strings.Join(excludes, ",") + "}"
	err := fs.WalkMode(root.ToString(), func(name string, isDir bool, info os.FileMode) error {
		excluded, err := doublestar.Match(excludePattern, filepath.ToSlash(name))
		if err != nil {
			return err
		} else if excluded {
			return godirwalk.SkipThis
		}

		if info.IsDir() && (info&os.ModeSymlink == 0) {
			return fw.Add(name)
		}
		return nil
	})
	return err
}

func (fw *fileWatcher) onFileAdded(name string) error {
	info, err := os.Lstat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// We can race with a file being added and removed. Ignore it
			return nil
		}
		return err
	}
	if info.IsDir() {
		if err := fw.watchRecursively(fs.AbsolutePath(name)); err != nil {
			return err
		}
	}
	return nil
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
			if ev.Op&fsnotify.Create != 0 {
				if err := fw.onFileAdded(ev.Name); err != nil {
					fw.logger.Warn(fmt.Sprintf("failed to handle adding %v: %v", ev.Name, err))
					continue
				}
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
	fileWatcher := &fileWatcher{
		Watcher: watcher,
		logger:  logger.Named("FileWatcher"),
	}
	globWatcher := globwatcher.New(logger.Named("GlobWatcher"), repoRoot)
	server := &Server{
		watcher:     fileWatcher,
		globWatcher: globWatcher,
	}
	server.watcher.AddClient(globWatcher)
	if err := server.watcher.watchRecursively(repoRoot); err != nil {
		return nil, errors.Wrapf(err, "watching %v", repoRoot)
	}
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
	err := s.globWatcher.WatchGlobs(req.Hash, req.OutputGlobs)
	if err != nil {
		return nil, err
	}
	return &NotifyOutputsWrittenResponse{}, nil
}

func (s *Server) GetChangedOutputs(ctx context.Context, req *GetChangedOutputsRequest) (*GetChangedOutputsResponse, error) {
	changedGlobs, err := s.globWatcher.GetChangedGlobs(req.Hash, req.OutputGlobs)
	if err != nil {
		return nil, err
	}
	return &GetChangedOutputsResponse{
		ChangedOutputGlobs: changedGlobs,
	}, nil
}
