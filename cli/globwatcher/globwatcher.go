package globwatcher

import (
	"errors"
	"fmt"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/go-hclog"
	"github.com/vercel/turborepo/cli/internal/fs"
	"github.com/vercel/turborepo/cli/internal/util"
)

var (
	// ErrClosed is returned when attempting to get changed globs after glob watching has closed
	ErrClosed = errors.New("glob watching is closed")
)

type GlobWatcher struct {
	logger   hclog.Logger
	repoRoot fs.AbsolutePath

	mu         sync.RWMutex // protects field below
	hashGlobs  map[string]util.Set
	globStatus map[string]util.Set // glob -> hashes where this glob hasn't changed

	closed bool
}

func New(logger hclog.Logger, repoRoot fs.AbsolutePath) *GlobWatcher {
	return &GlobWatcher{
		logger:     logger,
		repoRoot:   repoRoot,
		hashGlobs:  make(map[string]util.Set),
		globStatus: make(map[string]util.Set),
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

func (g *GlobWatcher) WatchGlobs(hash string, globs []string) error {
	if g.isClosed() {
		return ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hashGlobs[hash] = util.SetFromStrings(globs)
	for _, glob := range globs {
		existing, ok := g.globStatus[glob]
		if !ok {
			existing = make(util.Set)
		}
		existing.Add(hash)
		g.globStatus[glob] = existing
	}
	return nil
}

func (g *GlobWatcher) GetChangedGlobs(hash string, candidates []string) ([]string, error) {
	if g.isClosed() {
		return nil, ErrClosed
	}
	// hashGlobs tracks all of the unchanged globs for a given hash
	// If hashGlobs doesn't have our hash, either everything has changed,
	// or we were never tracking it. Either way, consider all the candidates
	// to be changed globs.
	g.mu.RLock()
	defer g.mu.RUnlock()
	globsToCheck, ok := g.hashGlobs[hash]
	if !ok {
		return candidates, nil
	}
	allGlobs := util.SetFromStrings(candidates)
	diff := allGlobs.Difference(globsToCheck)
	return diff.UnsafeListOfStrings(), nil
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
		// If this glob matches, we know that it has changed for every hash that included this glob.
		// So, we can delete this glob from every hash tracking it as well as stop watching this glob.
		// To stop watching, we unref each of the directories corresponding to this glob.
		if matches {
			delete(g.globStatus, glob)
			for hashUntyped := range hashStatus {
				hash := hashUntyped.(string)
				hashGlobs, ok := g.hashGlobs[hash]
				if !ok {
					g.logger.Warn(fmt.Sprintf("failed to find hash %v referenced from glob %v", hash, glob))
					continue
				}
				hashGlobs.Delete(glob)
				// If we've deleted the last glob for a hash, delete the whole hash entry
				if hashGlobs.Len() == 0 {
					delete(g.hashGlobs, hash)
				}
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
