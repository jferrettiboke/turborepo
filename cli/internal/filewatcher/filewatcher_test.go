package filewatcher

import (
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/go-hclog"
	turbofs "github.com/vercel/turborepo/cli/internal/fs"
	"github.com/vercel/turborepo/cli/internal/util"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/fs"
)

type testClient struct {
	mu     sync.Mutex
	events []fsnotify.Event
	notify chan<- struct{}
}

func (c *testClient) OnFileWatchEvent(ev fsnotify.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	c.notify <- struct{}{}
}

func (c *testClient) OnFileWatchError(err error) {}

func (c *testClient) OnFileWatchClosed() {}

func assertSameSet(t *testing.T, gotSlice []string, wantSlice []string) {
	got := util.SetFromStrings(gotSlice)
	want := util.SetFromStrings(wantSlice)
	extra := got.Difference(want)
	missing := want.Difference(got)
	if extra.Len() > 0 {
		t.Errorf("found extra elements: %v", extra.UnsafeListOfStrings())
	}
	if missing.Len() > 0 {
		t.Errorf("missing expected elements: %v", missing.UnsafeListOfStrings())
	}
}

func expectFilesystemEvent(t *testing.T, ch <-chan struct{}) {
	select {
	case <-ch:
		return
	case <-time.After(1 * time.Second):
		t.Error("Timed out waiting for filesystem event")
	}
}

func expectNoFilesystemEvent(t *testing.T, ch <-chan struct{}) {
	select {
	case ev := <-ch:
		t.Errorf("got unexpected filesystem event %v", ev)
	case <-time.After(100 * time.Millisecond):
		return
	}
}

func TestFileWatching(t *testing.T) {
	logger := hclog.Default()
	repoRootRaw := fs.NewDir(t, "server-test")
	repoRoot := turbofs.UnsafeToAbsolutePath(repoRootRaw.Path())
	err := repoRoot.Join(".git").MkdirAll()
	assert.NilError(t, err, "MkdirAll")
	err = repoRoot.Join("node_modules", "some-dep").MkdirAll()
	assert.NilError(t, err, "MkdirAll")
	err = repoRoot.Join("parent", "child").MkdirAll()
	assert.NilError(t, err, "MkdirAll")
	err = repoRoot.Join("parent", "sibling").MkdirAll()
	assert.NilError(t, err, "MkdirAll")

	// Directory layout:
	// <repoRoot>/
	//	 .git/
	//   node_modules/
	//     some-dep/
	//   parent/
	//     child/
	//     sibling/

	watcher, err := fsnotify.NewWatcher()
	assert.NilError(t, err, "NewWatcher")
	fw := New(logger, repoRoot, watcher)
	err = fw.Start()
	assert.NilError(t, err, "watchRecursively")
	expectedWatching := []string{
		repoRoot.ToString(),
		repoRoot.Join("parent").ToString(),
		repoRoot.Join("parent", "child").ToString(),
		repoRoot.Join("parent", "sibling").ToString(),
	}
	watching := fw.WatchList()
	assertSameSet(t, watching, expectedWatching)

	// Add a client
	ch := make(chan struct{})
	c := &testClient{
		notify: ch,
	}
	fw.AddClient(c)
	go fw.watch()

	fooPath := repoRoot.Join("parent", "child", "foo")
	err = fooPath.WriteFile([]byte("hello"), 0644)
	assert.NilError(t, err, "WriteFile")
	expectFilesystemEvent(t, ch)
	expectedEvent := fsnotify.Event{
		Op:   fsnotify.Create,
		Name: fooPath.ToString(),
	}
	c.mu.Lock()
	got := c.events[len(c.events)-1]
	c.mu.Unlock()
	assert.DeepEqual(t, got, expectedEvent)
	expectedWatching = append(expectedWatching, fooPath.ToString())
	watching = fw.WatchList()
	assertSameSet(t, watching, expectedWatching)

	deepPath := repoRoot.Join("parent", "sibling", "deep", "path")
	err = deepPath.MkdirAll()
	assert.NilError(t, err, "MkdirAll")
	// We'll catch an event for "deep", but not "deep/path" since
	// we don't have a recursive watch
	expectFilesystemEvent(t, ch)

	expectedWatching = append(expectedWatching, deepPath.ToString(), repoRoot.Join("parent", "sibling", "deep").ToString())
	watching = fw.WatchList()
	assertSameSet(t, watching, expectedWatching)

	gitFilePath := repoRoot.Join(".git", "git-file")
	err = gitFilePath.WriteFile([]byte("nope"), 0644)
	assert.NilError(t, err, "WriteFile")
	expectNoFilesystemEvent(t, ch)

	// No change in watchlist
	watching = fw.WatchList()
	assertSameSet(t, watching, expectedWatching)

}
