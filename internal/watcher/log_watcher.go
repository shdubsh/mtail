// Copyright 2015 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package watcher

import (
	"expvar"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

var (
	eventCount = expvar.NewMap("log_watcher_event_count")
	errorCount = expvar.NewInt("log_watcher_error_count")
)

// LogWatcher implements a Watcher for watching real filesystems.
type LogWatcher struct {
	watcher    *fsnotify.Watcher
	pollTicker *time.Ticker

	eventsMu sync.RWMutex
	events   []chan Event

	watchedMu sync.RWMutex          // protects `watched'
	watched   map[string]chan Event // Names of paths being watched

	stopTicks chan struct{} // Channel to notify ticker to stop.

	ticksDone  chan struct{} // Channel to notify when the ticks handler is done.
	eventsDone chan struct{} // Channel to notify when the events handler is done.

	closeOnce sync.Once
}

// NewLogWatcher returns a new LogWatcher, or returns an error.
func NewLogWatcher(pollInterval time.Duration, enableFsnotify bool) (*LogWatcher, error) {
	var f *fsnotify.Watcher
	if enableFsnotify {
		var err error
		f, err = fsnotify.NewWatcher()
		if err != nil {
			glog.Warning(err)
		}
	}
	if f == nil && pollInterval == 0 {
		pollInterval = time.Millisecond * 250
	}
	w := &LogWatcher{
		watcher: f,
		events:  make([]chan Event, 0),
		watched: make(map[string]chan Event),
	}
	if pollInterval > 0 {
		w.pollTicker = time.NewTicker(pollInterval)
		w.stopTicks = make(chan struct{})
		w.ticksDone = make(chan struct{})
		go w.runTicks()
	}
	if f != nil {
		w.eventsDone = make(chan struct{})
		go w.runEvents()
	}
	return w, nil
}

// Events returns a new readable channel of events from this watcher.
func (w *LogWatcher) Events() (int, <-chan Event) {
	w.eventsMu.Lock()
	handle := len(w.events)
	ch := make(chan Event)
	w.events = append(w.events, ch)
	w.eventsMu.Unlock()
	return handle, ch
}

func (w *LogWatcher) sendEvent(e Event) {
	w.watchedMu.RLock()
	c, ok := w.watched[e.Pathname]
	w.watchedMu.RUnlock()
	if !ok {
		d := filepath.Dir(e.Pathname)
		w.watchedMu.RLock()
		c, ok = w.watched[d]
		w.watchedMu.RUnlock()
	}
	if ok {
		c <- e
		return
	}
	glog.V(2).Infof("No channel for path %q", e.Pathname)
}

func (w *LogWatcher) runTicks() {
	defer close(w.ticksDone)

	if w.pollTicker == nil {
		return
	}

Exit:
	for {
		select {
		case _ = <-w.pollTicker.C:
			w.watchedMu.RLock()
			for n, c := range w.watched {
				c <- Event{Update, n}
			}
			w.watchedMu.RUnlock()
		case <-w.stopTicks:
			w.pollTicker.Stop()
			break Exit
		}
	}
}

// runEvents assumes that w.watcher is not nil
func (w *LogWatcher) runEvents() {
	defer close(w.eventsDone)

	// Suck out errors and dump them to the error log.
	go func() {
		for err := range w.watcher.Errors {
			errorCount.Add(1)
			glog.Errorf("fsnotify error: %s\n", err)
		}
	}()

	for e := range w.watcher.Events {
		glog.V(2).Infof("watcher event %v", e)
		eventCount.Add(e.Name, 1)
		switch {
		case e.Op&fsnotify.Create == fsnotify.Create:
			w.sendEvent(Event{Create, e.Name})
		case e.Op&fsnotify.Write == fsnotify.Write,
			e.Op&fsnotify.Chmod == fsnotify.Chmod:
			w.sendEvent(Event{Update, e.Name})
		case e.Op&fsnotify.Remove == fsnotify.Remove:
			w.sendEvent(Event{Delete, e.Name})
		case e.Op&fsnotify.Rename == fsnotify.Rename:
			// Rename is only issued on the original file path; the new name receives a Create event
			w.sendEvent(Event{Delete, e.Name})
		default:
			panic(fmt.Sprintf("unknown op type %v", e.Op))
		}
	}
	glog.Infof("Shutting down log watcher.")
}

// Close shuts down the LogWatcher.  It is safe to call this from multiple clients.
func (w *LogWatcher) Close() (err error) {
	w.closeOnce.Do(func() {
		if w.watcher != nil {
			err = w.watcher.Close()
			<-w.eventsDone
		}
		if w.pollTicker != nil {
			close(w.stopTicks)
			<-w.ticksDone
		}
		glog.Info("Closing events channels")
		w.eventsMu.Lock()
		for _, c := range w.events {
			close(c)
		}
		w.eventsMu.Unlock()
	})
	return nil
}

// Add adds a path to the list of watched items.
// If the path is already being watched, then nothing is changed -- the new handle does not replace the old one.
func (w *LogWatcher) Add(path string, handle int) error {
	w.eventsMu.RLock()
	if handle > len(w.events) {
		return errors.Errorf("no such event handle %d", handle)
	}
	w.eventsMu.RUnlock()
	if w.IsWatching(path) {
		return nil
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return errors.Wrapf(err, "Failed to lookup absolutepath of %q", path)
	}
	// avoid setting watches on /dev.
	if absPath == "/dev" {
		glog.V(2).Info("Skipping adding watch on /dev")
	} else {
		glog.V(2).Infof("Adding a watch on resolved path %q", absPath)
		err = w.watcher.Add(absPath)
		if err != nil {
			if os.IsPermission(err) {
				glog.V(2).Infof("Skipping permission denied error on adding a watch.")
			} else {
				return errors.Wrapf(err, "Failed to create a new watch on %q", absPath)
			}
		}
	}
	w.watchedMu.Lock()
	w.eventsMu.RLock()
	w.watched[absPath] = w.events[handle]
	w.eventsMu.RUnlock()
	w.watchedMu.Unlock()
	return nil
}

// IsWatching indicates if the path is being watched. It includes both
// filenames and directories.
func (w *LogWatcher) IsWatching(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		glog.V(2).Infof("Couldn't resolve path %q: %s", absPath, err)
		return false
	}
	glog.V(2).Infof("Resolved path for lookup %q", absPath)
	w.watchedMu.RLock()
	_, ok := w.watched[absPath]
	w.watchedMu.RUnlock()
	return ok
}

func (w *LogWatcher) Remove(path string) error {
	w.watchedMu.Lock()
	delete(w.watched, path)
	w.watchedMu.Unlock()
	if w.watcher != nil {
		return w.watcher.Remove(path)
	}
	return nil
}
