package globwatcher

import (
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/go-hclog"
	turbofs "github.com/vercel/turborepo/cli/internal/fs"
	"github.com/vercel/turborepo/cli/internal/util"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/fs"
)

type testWatcher struct {
	watches util.Set
}

func newTestWatcher() *testWatcher {
	return &testWatcher{
		watches: make(util.Set),
	}
}

func (tw *testWatcher) Add(name string) error {
	tw.watches.Add(name)
	return nil
}

func (tw *testWatcher) Remove(name string) error {
	tw.watches.Delete(name)
	return nil
}

func setup(t *testing.T, repoRoot turbofs.AbsolutePath) {
	// Directory layout:
	// <repoRoot>/
	//   my-pkg/
	//     irrelevant
	//     dist/
	//       dist-file
	//       distChild/
	//         child-file
	//     .next/
	//       next-file
	distPath := repoRoot.Join("my-pkg", "dist")
	childFilePath := distPath.Join("distChild", "child-file")
	err := childFilePath.EnsureDir()
	assert.NilError(t, err, "EnsureDir")
	f, err := childFilePath.Create()
	assert.NilError(t, err, "Create")
	err = f.Close()
	assert.NilError(t, err, "Close")
	distFilePath := repoRoot.Join("my-pkg", "dist", "dist-file")
	f, err = distFilePath.Create()
	assert.NilError(t, err, "Create")
	err = f.Close()
	assert.NilError(t, err, "Close")
	nextFilePath := repoRoot.Join("my-pkg", ".next", "next-file")
	err = nextFilePath.EnsureDir()
	assert.NilError(t, err, "EnsureDir")
	f, err = nextFilePath.Create()
	assert.NilError(t, err, "Create")
	err = f.Close()
	assert.NilError(t, err, "Close")
	irrelevantPath := repoRoot.Join("my-pkg", "irrelevant")
	f, err = irrelevantPath.Create()
	assert.NilError(t, err, "Create")
	err = f.Close()
	assert.NilError(t, err, "Close")
}

func TestTrackOutputs(t *testing.T) {
	logger := hclog.Default()

	repoRootRaw := fs.NewDir(t, "globwatcher-test")
	repoRoot := turbofs.UnsafeToAbsolutePath(repoRootRaw.Path())

	setup(t, repoRoot)

	watcher := newTestWatcher()
	globWatcher := New(logger, repoRoot, watcher)

	globs := []string{
		"my-pkg/dist/**",
		"my-pkg/.next/**",
	}
	hash := "the-hash"
	err := globWatcher.WatchGlobs(hash, globs)
	assert.NilError(t, err, "WatchGlobs")

	assert.Equal(t, 3, len(watcher.watches))
	expectedWatches := []string{
		repoRoot.Join("my-pkg", "dist").ToString(),
		repoRoot.Join("my-pkg", "dist", "distChild").ToString(),
		repoRoot.Join("my-pkg", ".next").ToString(),
	}
	for _, expected := range expectedWatches {
		if !watcher.watches.Includes(expected) {
			t.Errorf("expected to watch %v, found %v", expected, watcher.watches.UnsafeListOfStrings())
		}
	}

	changed, err := globWatcher.GetChangedGlobs(hash, globs)
	assert.NilError(t, err, "GetChangedGlobs")
	assert.Equal(t, 0, len(changed), "Expected no changed paths")

	// Make an irrelevant change
	globWatcher.OnFileWatchEvent(fsnotify.Event{
		Op:   fsnotify.Create,
		Name: repoRoot.Join("my-pkg", "irrelevant").ToString(),
	})

	changed, err = globWatcher.GetChangedGlobs(hash, globs)
	assert.NilError(t, err, "GetChangedGlobs")
	assert.Equal(t, 0, len(changed), "Expected no changed paths")

	// Make a relevant change
	globWatcher.OnFileWatchEvent(fsnotify.Event{
		Op:   fsnotify.Create,
		Name: repoRoot.Join("my-pkg", "dist", "foo").ToString(),
	})

	changed, err = globWatcher.GetChangedGlobs(hash, globs)
	assert.NilError(t, err, "GetChangedGlobs")
	assert.Equal(t, 1, len(changed), "Expected one changed path remaining")
	expected := "my-pkg/dist/**"
	assert.Equal(t, expected, changed[0], "Expected dist glob to have changed")

	// Now that that glob has changed, we should've stopped watching it
	distWatches := []string{
		repoRoot.Join("my-pkg", "dist").ToString(),
		repoRoot.Join("my-pkg", "dist", "distChild").ToString(),
	}
	for _, distWatch := range distWatches {
		if watcher.watches.Includes(distWatch) {
			t.Errorf("expected to no longer be watching %v", distWatch)
		}
	}

	// Change a file matching the other glob
	globWatcher.OnFileWatchEvent(fsnotify.Event{
		Op:   fsnotify.Create,
		Name: repoRoot.Join("my-pkg", ".next", "foo").ToString(),
	})

	// Both globs have changed, we should have stopped tracking
	// this hash
	changed, err = globWatcher.GetChangedGlobs(hash, globs)
	assert.NilError(t, err, "GetChangedGlobs")
	assert.DeepEqual(t, globs, changed)

	// We should no longer be watching anything, since both globs have
	// registered changes
	if watcher.watches.Len() != 0 {
		t.Errorf("expected to no longer have watches, but still watching %v", watcher.watches.UnsafeListOfStrings())
	}
}

func TestWatchSingleFile(t *testing.T) {
	logger := hclog.Default()

	repoRootRaw := fs.NewDir(t, "globwatcher-test")
	repoRoot := turbofs.UnsafeToAbsolutePath(repoRootRaw.Path())

	setup(t, repoRoot)

	watcher := newTestWatcher()
	globWatcher := New(logger, repoRoot, watcher)
	globs := []string{
		"my-pkg/.next/next-file",
	}
	hash := "the-hash"
	err := globWatcher.WatchGlobs(hash, globs)
	assert.NilError(t, err, "WatchGlobs")

	assert.Equal(t, 1, len(watcher.watches))
	expectedWatches := []string{
		repoRoot.Join("my-pkg", ".next", "next-file").ToString(),
	}
	for _, expected := range expectedWatches {
		if !watcher.watches.Includes(expected) {
			t.Errorf("expected to watch %v, found %v", expected, watcher.watches.UnsafeListOfStrings())
		}
	}
}
