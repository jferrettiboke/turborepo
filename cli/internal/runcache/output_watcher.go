package runcache

type OutputWatcher interface {
	GetChangedOutputs(hash string, repoRelativeOutputGlobs []string) ([]string, error)
	// MarkSaved tells the watcher that the given globs have been cached with the specified hash
	NotifyOutputsWritten(hash string, repoRelativeOutputGlobs []string) error
}

type NoOpOutputWatcher struct{}

var _ OutputWatcher = &NoOpOutputWatcher{}

func (NoOpOutputWatcher) GetChangedOutputs(hash string, repoRelativeOutputGlobs []string) ([]string, error) {
	return repoRelativeOutputGlobs, nil
}

// MarkSaved implements OutputWatcher.MarkSaved
func (NoOpOutputWatcher) NotifyOutputsWritten(hash string, repoRelativeOutputGlobs []string) error {
	return nil
}
