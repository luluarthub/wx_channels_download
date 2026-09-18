package hermes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitTaskJob(t *testing.T) {
	if !wait_task_job(nil, 0) {
		t.Fatal("absent task should already be stopped")
	}
	done := make(chan struct{})
	close(done)
	if !wait_task_job(&TaskJob{done: done}, 0) {
		t.Fatal("completed task must win even with an expired deadline")
	}
	if wait_task_job(&TaskJob{done: make(chan struct{})}, 10*time.Millisecond) {
		t.Fatal("unfinished task must time out")
	}
}

type cancellation_test_store struct {
	Store
	status_calls int
	status_error error
}

func (s *cancellation_test_store) UpdateStatus(task_id, status int) error {
	s.status_calls++
	return s.status_error
}

func TestTaskCancellationTimeoutKeepsWorkerAndRecords(t *testing.T) {
	for _, operation := range []string{"pause", "delete"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			store := &cancellation_test_store{}
			d := New(HermesNewConfig{Store: store})
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			job := &TaskJob{ID: 1, ctx: ctx, cancel: cancel, done: make(chan struct{})}
			d.jobs[1] = job
			started := time.Now()
			var err error
			if operation == "pause" {
				err = d.PauseTask(1)
			} else {
				err = d.DeleteTask(1)
			}
			if err == nil || !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("expected actionable timeout, got %v", err)
			}
			if time.Since(started) > task_stop_timeout+5*time.Second {
				t.Fatal("cancellation exceeded the bounded wait")
			}
			if ctx.Err() == nil || d.find_job(1) != job {
				t.Fatal("timed-out worker must remain registered and receive cancellation")
			}
			if store.status_calls != 0 {
				t.Fatal("must not mark cancellation successful while the worker can still write")
			}
			if err := d.StartTask(1); err != nil {
				t.Fatalf("duplicate start: %v", err)
			}
			if d.find_job(1) != job || store.status_calls != 0 {
				t.Fatal("resume must not replace an unfinished worker")
			}
			close(job.done)
			if operation == "delete" {
				if err := d.DeleteTask(1); err != nil || store.status_calls != 1 {
					t.Fatalf("deletion can be retried after worker exits: calls=%d err=%v", store.status_calls, err)
				}
			}
		})
	}
}

func TestDeleteTaskPropagatesStatusPersistenceError(t *testing.T) {
	want := errors.New("database unavailable")
	store := &cancellation_test_store{status_error: want}
	d := New(HermesNewConfig{Store: store})
	done := make(chan struct{})
	close(done)
	d.jobs[7] = &TaskJob{ID: 7, done: done}
	if err := d.DeleteTask(7); !errors.Is(err, want) {
		t.Fatalf("got %v, want persisted cancellation failure", err)
	}
}

func TestSegmentCountPreservesBoundedSplits(t *testing.T) {
	for _, test := range []struct {
		size int64
		want int
	}{{100 * 1024 * 1024, 8}, {2 * 1024 * 1024 * 1024, 16}} {
		if got := choose_segment_count(PreparedResource{Size: test.size, SupportsRange: true}); got != test.want {
			t.Errorf("size %d: got %d segments, want %d", test.size, got, test.want)
		}
	}
	if got := choose_segment_count(PreparedResource{Size: 2 * 1024 * 1024 * 1024}); got != 1 {
		t.Fatalf("non-range server must use one stream, got %d", got)
	}
}
