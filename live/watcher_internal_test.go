package live

import (
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestAttributeOnlyEventsAreNotQueued(t *testing.T) {
	w := &Watcher{pending: map[string]struct{}{}}
	w.note(fsnotify.Event{Name: "/repo/a.go", Op: fsnotify.Chmod})
	if len(w.pending) != 0 || w.timer != nil {
		t.Fatalf("a chmod-only event was queued: %v", w.pending)
	}
	w.note(fsnotify.Event{Name: "/repo/a.go", Op: fsnotify.Write | fsnotify.Chmod})
	if _, ok := w.pending["/repo/a.go"]; !ok {
		t.Fatal("a write that also changed attributes must still be queued")
	}
	w.timer.Stop()
}
