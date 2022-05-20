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
	"github.com/vercel/turborepo/cli/internal/util"
)

var (
	// ErrClosed is returned when attempting to get changed globs after glob watching has closed
	ErrClosed      = errors.New("glob watching is closed")
	ErrConsistency = errors.New("internal consistency error. The hash lists the glob, but we are not tracking it")
)

type FileWatcher interface {
	Add(name string) error
	Remove(name string) error
}

type GlobWatcher struct {
	logger      hclog.Logger
	repoRoot    fs.AbsolutePath
	fileWatcher FileWatcher

	mu         sync.RWMutex // protects field below
	hashGlobs  map[string]util.Set
	globStatus map[string]util.Set // glob -> hashes where this glob hasn't changed

	dirRefs  map[string]int      // directory being watch -> ref count
	globDirs map[string][]string // glob -> directories watched on account of that glob
	closed   bool
}

func New(logger hclog.Logger, repoRoot fs.AbsolutePath, fileWatcher FileWatcher) *GlobWatcher {
	return &GlobWatcher{
		logger:      logger,
		repoRoot:    repoRoot,
		fileWatcher: fileWatcher,
		hashGlobs:   make(map[string]util.Set),
		globStatus:  make(map[string]util.Set),
		globDirs:    make(map[string][]string),
		dirRefs:     make(map[string]int),
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
		if err := g.watchRecursively(glob); err != nil {
			return err
		}
		existing, ok := g.globStatus[glob]
		if !ok {
			existing = make(util.Set)
		}
		existing.Add(hash)
		g.globStatus[glob] = existing
	}
	return nil
}

var _aferoIOFS = afero.NewIOFS(afero.NewOsFs())

func (g *GlobWatcher) watchRecursively(glob string) error {
	if dirs, ok := g.globDirs[glob]; ok {
		// if we're already watching this glob, just bump the ref count
		for _, dir := range dirs {
			g.refDir(dir)
		}
		return nil
	}
	includePattern := g.repoRoot.Join(glob).ToString()

	var dirs []string
	// TODO(gsoltis): given a glob, what do we watch?
	err := doublestar.GlobWalk(_aferoIOFS, includePattern, func(path string, d iofs.DirEntry) error {
		if d.IsDir() {
			g.logger.Debug(fmt.Sprintf("start watching %v", path))
			g.refDir(path)
			dirs = append(dirs, path)
			return g.fileWatcher.Add(path)
		}
		return nil
	})
	g.globDirs[glob] = dirs
	return err
}

func (g *GlobWatcher) refDir(dir string) {
	count, ok := g.dirRefs[dir]
	if !ok {
		count = 0
	}
	g.dirRefs[dir] = count + 1
}

func (g *GlobWatcher) unrefDir(dir string) {
	count, ok := g.dirRefs[dir]
	if !ok {
		g.logger.Warn(fmt.Sprintf("trying to unref dir we are not tracking: %v", dir))
		return
	}
	count--
	if count == 0 {
		delete(g.dirRefs, dir)
		if err := g.fileWatcher.Remove(dir); err != nil {
			g.logger.Warn(fmt.Sprintf("failed to stop watching directory %v: %v", dir, err))
		}
	}
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
			globDirs, ok := g.globDirs[glob]
			if !ok {
				g.logger.Warn(fmt.Sprintf("failed to find directories tracked for glob %v", glob))
				continue
			}
			for _, dir := range globDirs {
				g.unrefDir(dir)
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
