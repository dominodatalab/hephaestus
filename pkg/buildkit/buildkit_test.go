package buildkit

import (
	"context"
	"errors"
	"testing"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/stretchr/testify/require"
)

// TestRunProgressSolve_ProducerForgetsToCloseChannel verifies the root-cause fix for the
// "ImageBuild stuck in Running after push completes" bug. Buildkit's client is supposed to close
// the progress channel when Solve returns, but in production we've observed it returning without
// closing — which used to hang the consumer forever. The sync.Once close guard in
// runProgressSolve must unblock the consumer so the whole operation returns cleanly.
func TestRunProgressSolve_ProducerForgetsToCloseChannel(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := runProgressSolve(
			context.Background(),
			func(_ context.Context, _ chan *bkclient.SolveStatus) error {
				// simulate buildkit "forgetting" to close the channel: return nil without sending
				// anything and without closing.
				return nil
			},
			func(_ context.Context, ch chan *bkclient.SolveStatus) error {
				// drain until closed. Before the fix this blocks forever.
				for range ch {
				}
				return nil
			},
		)
		require.NoError(t, err)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runProgressSolve hung: producer returned without closing channel and consumer never unblocked")
	}
}

// TestRunProgressSolve_ProducerErrorStillCloses verifies the guard also triggers on the error path.
func TestRunProgressSolve_ProducerErrorStillCloses(t *testing.T) {
	sentinel := errors.New("solve failed")

	done := make(chan struct{})
	var gotErr error
	go func() {
		defer close(done)
		gotErr = runProgressSolve(
			context.Background(),
			func(_ context.Context, _ chan *bkclient.SolveStatus) error {
				return sentinel
			},
			func(_ context.Context, ch chan *bkclient.SolveStatus) error {
				for range ch {
				}
				return nil
			},
		)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runProgressSolve hung on producer error path")
	}
	require.ErrorIs(t, gotErr, sentinel)
}

// TestRunProgressSolve_ConsumerReceivesUpdates is a smoke test that the happy path still works:
// producer sends status updates, closes the channel, both goroutines exit cleanly.
func TestRunProgressSolve_ConsumerReceivesUpdates(t *testing.T) {
	var received int

	err := runProgressSolve(
		context.Background(),
		func(_ context.Context, ch chan *bkclient.SolveStatus) error {
			for i := 0; i < 3; i++ {
				ch <- &bkclient.SolveStatus{}
			}
			return nil
		},
		func(_ context.Context, ch chan *bkclient.SolveStatus) error {
			for range ch {
				received++
			}
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 3, received)
}
