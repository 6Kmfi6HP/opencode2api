package app

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestAdminReloadSingleflight(t *testing.T) {
	const callers = 8
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	results := make(chan adminReloadResult, callers)
	reload := func() adminReloadResult {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return adminReloadResult{sessionID: "ses_test", freeCount: 1, goCount: 1}
	}

	for range callers {
		go func() { results <- doAdminReload(reload) }()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("reload did not start")
	}
	select {
	case <-results:
		close(release)
		t.Fatal("concurrent reload did not share the in-flight result")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	for range callers {
		if result := <-results; result.sessionID != "ses_test" {
			t.Fatalf("unexpected reload result: %+v", result)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("reload calls = %d, want 1", got)
	}
}
