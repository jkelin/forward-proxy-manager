package main

import (
	"context"
	"testing"
	"time"
)

// TestServersStopWithContext checks every listener started by main returns once ctx is cancelled.
// Port 0 picks a free port, so the servers never clash with a running instance.
func TestServersStopWithContext(t *testing.T) {
	for _, testCase := range []struct {
		name string
		run  func(ctx context.Context, scheduler *RequestScheduler, port uint16)
	}{
		{"grpc", runGrpcProxy},
		{"http proxy", runHttpProxy},
		{"web", runWeb},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newTestScheduler(0)
			startScheduler(t, scheduler)

			ctx, cancel := context.WithCancel(context.Background())
			stopped := make(chan struct{})

			go func() {
				defer close(stopped)
				testCase.run(ctx, scheduler, 0)
			}()

			time.Sleep(100 * time.Millisecond)
			cancel()

			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("server did not stop after ctx was cancelled")
			}
		})
	}
}
