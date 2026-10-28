package eventnotify

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// What was emitted just before Close is still sent: Close must not let the
// sending goroutine stop while one-shot alerts or queued Events remain (a
// one-shot reconcile emits and closes right away; see [AC-131-4]).
func TestClose_SendsWhatWasEmittedJustBefore(t *testing.T) {
	rv := &recv{}
	srv := httptest.NewServer(rv)
	defer srv.Close()
	for i := 0; i < 300; i++ {
		var errOut syncBuffer
		o := fastOpts
		o.ErrOut = &errOut
		a, err := NewAlertmanager(Target{URL: srv.URL + "/"}, "", o)
		if err != nil {
			t.Fatal(err)
		}
		w, err := NewWebhook(Target{URL: srv.URL + "/"}, "", o)
		if err != nil {
			t.Fatal(err)
		}
		// Give the goroutines a moment to go idle, as in a reconcile run.
		if i%2 == 0 {
			time.Sleep(time.Millisecond)
		}
		_ = a.Emit(context.Background(), driftEvent(core.EventFiring))
		_ = a.Emit(context.Background(), driftEvent(core.EventResolved))
		_ = w.Emit(context.Background(), driftEvent(core.EventFiring))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := a.Close(ctx); err != nil {
			t.Fatalf("round %d: alertmanager Close: %v", i, err)
		}
		if err := w.Close(ctx); err != nil {
			t.Fatalf("round %d: webhook Close: %v", i, err)
		}
		cancel()
	}
}
