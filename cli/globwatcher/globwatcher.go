package globwatcher

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/go-hclog"
	"github.com/spf13/afero"
	"github.com/vercel/turborepo/cli/internal/fs"
)

var (
	// ErrClosed is returned when attempting to get changed globs after glob watching has closed
	ErrClosed = errors.New("glob watching is closed")
	// ErrNotTracked is returned when GlobWatcher does not have a record of the requested hash
	ErrNotTracked  = errors.New("the given hash is not being tracked")
	ErrConsistency = errors.New("internal consistency error. The hash lists the glob, but we are not tracking it")
)

type FileWatcher interface {
	Add(name string) error
	Remove(name string) error
}

type GlobWatcher struct {
	logger   hclog.Logger
	repoRoot fs.AbsolutePath

	mu         sync.RWMutex // protects field below
	hashGlobs  map[string][]string
	globStatus map[string]map[string]bool // glob -> hash -> has changed
	closed     bool
}

func New(logger hclog.Logger, repoRoot fs.AbsolutePath) *GlobWatcher {
	return &GlobWatcher{
		logger:     logger,
		repoRoot:   repoRoot,
		hashGlobs:  make(map[string][]string),
		globStatus: make(map[string]map[string]bool),
	}
}

func (g *GlobWatcher) setClosed() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

func (g *GlobWatcher) isClosed() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.closed
}

func (g *GlobWatcher) WatchGlobs(watcher FileWatcher, hash string, globs []string) error {
	if g.isClosed() {
		return ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hashGlobs[hash] = globs
	for _, glob := range globs {
		if err := g.watchRecursively(watcher, glob); err != nil {
			return err
		}
		existing, ok := g.globStatus[glob]
		if !ok {
			existing = make(map[string]bool)
		}
		existing[hash] = false
		g.globStatus[glob] = existing
	}
	return nil
}

var _aferoIOFS = afero.NewIOFS(afero.NewOsFs())

func (g *GlobWatcher) watchRecursively(watcher FileWatcher, glob string) error {
	includePattern := g.repoRoot.Join(glob).ToString()

	err := doublestar.GlobWalk(_aferoIOFS, includePattern, func(path string, d iofs.DirEntry) error {
		if d.IsDir() {
			g.logger.Debug(fmt.Sprintf("start watching %v", path))
			return watcher.Add(path)
		}
		return nil
	})
	return err
}

func (g *GlobWatcher) GetChangedGlobs(hash string) ([]string, error) {
	if g.isClosed() {
		return nil, ErrClosed
	}
	// First, we look up the set of globs for a hash in hashGlobs
	// Then, for each glob, check if it has changed since we started
	// tracking that particular hash.
	var changedGlobs []string
	g.mu.RLock()
	defer g.mu.RUnlock()
	globsToCheck, ok := g.hashGlobs[hash]
	if !ok {
		return nil, ErrNotTracked
	}
	for _, glob := range globsToCheck {
		hashes, ok := g.globStatus[glob]
		if !ok {
			return nil, ErrConsistency
		}
		hasChanged, ok := hashes[hash]
		if !ok {
			return nil, ErrConsistency
		}
		if hasChanged {
			changedGlobs = append(changedGlobs, glob)
		}
	}
	return changedGlobs, nil
}

func (g *GlobWatcher) OnFileWatchEvent(ev fsnotify.Event) {
	// At this point, we don't care what the Op is, any Op represents a change
	// that should invalidate matching globs
	g.logger.Debug(fmt.Sprintf("Got fsnotify event %v", ev))
	absolutePath := ev.Name
	repoRelativePath, err := g.repoRoot.RelativePathString(absolutePath)
	if err != nil {
		g.logger.Error(fmt.Sprintf("could not get relative path from %v to %v: %v", g.repoRoot, absolutePath, err))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for glob, hashStatus := range g.globStatus {
		matches, err := doublestar.Match(glob, repoRelativePath)
		if err != nil {
			g.logger.Error(fmt.Sprintf("failed to check path %v against glob %v: %v", repoRelativePath, glob, err))
			continue
		}
		if matches {
			for hash := range hashStatus {
				hashStatus[hash] = true
			}
		}
	}
}

func (g *GlobWatcher) OnFileWatchError(err error) {
	g.logger.Error(fmt.Sprintf("file watching received an error: %v", err))
}

// OnFileWatchingClosed implements FileWatchClient.OnFileWatchingClosed
func (g *GlobWatcher) OnFileWatchingClosed() {
	g.setClosed()
	g.logger.Warn("GlobWatching is closing due to file watching closing")
}
