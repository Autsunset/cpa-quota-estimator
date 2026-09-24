package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSlowAttributionWriterDoesNotHoldAppLock(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "lock.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	entered := make(chan struct{})
	release := make(chan struct{})
	a := &app{store: s, cfg: defaultConfig()}
	a.attributionWriter = func(context.Context, *store, *weightFit, int64) (int64, error) {
		close(entered)
		<-release
		return 0, nil
	}
	fit := &weightFit{Available: true}
	done := make(chan struct{})
	go func() { a.applyFittedWeights(context.Background(), s, fit); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("attribution writer did not start")
	}
	lockAvailable := make(chan struct{})
	go func() { a.mu.RLock(); a.mu.RUnlock(); close(lockAvailable) }()
	select {
	case <-lockAvailable:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("app lock was held during slow attribution I/O")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fit application did not finish")
	}
}
