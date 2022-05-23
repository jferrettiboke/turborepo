package server

import (
	context "context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
	"github.com/vercel/turborepo/cli/internal/doublestar"
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
	watcher     *fileWatcher
	globWatcher *globwatcher.GlobWatcher
}

// TODO(gsoltis): move this into its own package
// fileWatcher handles watching all of the files in the monorepo.
// We currently ignore .git and top-level node_modules. We can revisit
// if necessary.
type fileWatcher struct {
	*fsnotify.Watcher

	logger         hclog.Logger
	repoRoot       fs.AbsolutePath
	excludePattern string

	clientsMu sync.RWMutex
	clients   []FileWatchClient
	closed    bool
}

func newFileWatcher(logger hclog.Logger, repoRoot fs.AbsolutePath, watcher *fsnotify.Watcher) *fileWatcher {
	excludes := make([]string, len(_ignores))
	for i, ignore := range _ignores {
		excludes[i] = filepath.FromSlash(repoRoot.Join(ignore).ToString() + "/**")
	}
	excludePattern := "{" + strings.Join(excludes, ",") + "}"
	return &fileWatcher{
		Watcher:        watcher,
		logger:         logger,
		repoRoot:       repoRoot,
		excludePattern: excludePattern,
	}
}

// _ignores is the set of paths we exempt from file-watching
var _ignores = []string{".git", "node_modules"}

func (fw *fileWatcher) watchRecursively(root fs.AbsolutePath) error {
	err := fs.WalkMode(root.ToString(), func(name string, isDir bool, info os.FileMode) error {
		if info.IsDir() && (info&os.ModeSymlink == 0) {
			return fw.Add(name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if root == fw.repoRoot {
		// Revoke the ignored directories, which are automatically added
		// because they are children of watched directories.
		for _, dir := range fw.WatchList() {
			excluded, err := doublestar.Match(fw.excludePattern, filepath.ToSlash(dir))
			if err != nil {
				return err
			}
			if excluded {
				if err := fw.Remove(dir); err != nil {
					return err
				}
			}
		}
	}
	return nil
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

// watch is the main file-watching loop. Watching is not recursive,
// so when new directories are added, they are manually recursively watched.
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
		client.OnFileWatchClosed()
	}
	fw.clientsMu.Unlock()
}

func (fw *fileWatcher) AddClient(client FileWatchClient) {
	fw.clientsMu.Lock()
	defer fw.clientsMu.Unlock()
	fw.clients = append(fw.clients, client)
	if fw.closed {
		client.OnFileWatchClosed()
	}
}

// FileWatchClient defines the callbacks used by the file watching loop.
// All methods are called from the same goroutine so they:
// 1) do not need synchronization
// 2) should minimize the work they are doing when called, if possible
type FileWatchClient interface {
	OnFileWatchEvent(ev fsnotify.Event)
	OnFileWatchError(err error)
	OnFileWatchClosed()
}

// New returns a new instance of Server
func New(logger hclog.Logger, repoRoot fs.AbsolutePath) (*Server, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fileWatcher := newFileWatcher(logger.Named("FileWatcher"), repoRoot, watcher)
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
