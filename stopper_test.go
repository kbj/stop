// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)
//
// Modified by: Klaus Birkelund Jensen (birkelund@gmail.com)

package stop_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birkelund/caller"
	"github.com/birkelund/stop"
	"github.com/pkg/errors"

	"golang.org/x/net/context"
)

func TestStopper(t *testing.T) {
	s := stop.NewStopper()
	running := make(chan struct{})
	waiting := make(chan struct{})
	cleanup := make(chan struct{})
	ctx := context.Background()

	s.RunWorker(ctx, func(context.Context) {
		<-running
	})

	go func() {
		s.Stop(ctx)
		close(waiting)
		<-cleanup
	}()

	<-s.ShouldStop()
	select {
	case <-waiting:
		t.Fatal("expected stopper to have blocked")
	case <-time.After(100 * time.Millisecond):
		// Expected.
	}
	close(running)
	select {
	case <-waiting:
		// Success.
	case <-time.After(time.Second):
		t.Fatal("stopper should have finished waiting")
	}
	close(cleanup)
}

type blockingCloser struct {
	block chan struct{}
}

func newBlockingCloser() *blockingCloser {
	return &blockingCloser{block: make(chan struct{})}
}

func (bc *blockingCloser) Unblock() {
	close(bc.block)
}

func (bc *blockingCloser) Close() {
	<-bc.block
}

func TestStopperIsStopped(t *testing.T) {
	s := stop.NewStopper()
	bc := newBlockingCloser()
	s.AddCloser(bc)
	go s.Stop(context.Background())

	select {
	case <-s.ShouldStop():
	case <-time.After(time.Second):
		t.Fatal("stopper should have finished waiting")
	}
	select {
	case <-s.IsStopped():
		t.Fatal("expected blocked closer to prevent stop")
	case <-time.After(100 * time.Millisecond):
		// Expected.
	}
	bc.Unblock()
	select {
	case <-s.IsStopped():
		// Expected
	case <-time.After(time.Second):
		t.Fatal("stopper should have finished stopping")
	}
}

func TestStopperMultipleStopees(t *testing.T) {
	const count = 3
	s := stop.NewStopper()
	ctx := context.Background()

	for i := 0; i < count; i++ {
		s.RunWorker(ctx, func(context.Context) {
			<-s.ShouldStop()
		})
	}

	done := make(chan struct{})
	go func() {
		s.Stop(ctx)
		close(done)
	}()

	<-done
}

func TestStopperStartFinishTasks(t *testing.T) {
	s := stop.NewStopper()
	ctx := context.Background()

	if err := s.RunTask(ctx, func(ctx context.Context) {
		go s.Stop(ctx)

		select {
		case <-s.ShouldStop():
			t.Fatal("expected stopper to be quiesceing")
		case <-time.After(100 * time.Millisecond):
			// Expected.
		}
	}); err != nil {
		t.Error(err)
	}
	select {
	case <-s.ShouldStop():
		// Success.
	case <-time.After(time.Second):
		t.Fatal("stopper should be ready to stop")
	}
}

func TestStopperRunWorker(t *testing.T) {
	s := stop.NewStopper()
	ctx := context.Background()
	s.RunWorker(ctx, func(context.Context) {
		<-s.ShouldStop()
	})
	closer := make(chan struct{})
	go func() {
		s.Stop(ctx)
		close(closer)
	}()
	select {
	case <-closer:
		// Success.
	case <-time.After(time.Second):
		t.Fatal("stopper should be ready to stop")
	}
}

// TestStopperQuiesce tests coordinate quiesce with Quiesce.
func TestStopperQuiesce(t *testing.T) {
	var stoppers []*stop.Stopper
	for i := 0; i < 3; i++ {
		stoppers = append(stoppers, stop.NewStopper())
	}
	var quiesceDone []chan struct{}
	var runTaskDone []chan struct{}
	ctx := context.Background()

	for _, s := range stoppers {
		thisStopper := s
		qc := make(chan struct{})
		quiesceDone = append(quiesceDone, qc)
		sc := make(chan struct{})
		runTaskDone = append(runTaskDone, sc)
		thisStopper.RunWorker(ctx, func(ctx context.Context) {
			// Wait until Quiesce() is called.
			<-qc
			err := thisStopper.RunTask(ctx, func(context.Context) {})
			if err != stop.ErrUnavailable {
				t.Error(err)
			}
			// Make the stoppers call Stop().
			close(sc)
			<-thisStopper.ShouldStop()
		})
	}

	done := make(chan struct{})
	go func() {
		for _, s := range stoppers {
			s.Quiesce(ctx)
		}
		// Make the tasks call RunTask().
		for _, qc := range quiesceDone {
			close(qc)
		}

		// Wait until RunTask() is called.
		for _, sc := range runTaskDone {
			<-sc
		}

		for _, s := range stoppers {
			s.Stop(ctx)
			<-s.IsStopped()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Errorf("timed out waiting for stop")
	}
}

type testCloser bool

func (tc *testCloser) Close() {
	*tc = true
}

func TestStopperClosers(t *testing.T) {
	s := stop.NewStopper()
	var tc1, tc2 testCloser
	s.AddCloser(&tc1)
	s.AddCloser(&tc2)
	s.Stop(context.Background())
	if !bool(tc1) || !bool(tc2) {
		t.Errorf("expected true & true; got %t & %t", tc1, tc2)
	}
}

func TestStopperNumTasks(t *testing.T) {
	s := stop.NewStopper()
	var tasks []chan bool
	for i := 0; i < 3; i++ {
		c := make(chan bool)
		tasks = append(tasks, c)
		if err := s.RunAsyncTask(context.Background(), func(_ context.Context) {
			// Wait for channel to close
			<-c
		}); err != nil {
			t.Fatal(err)
		}
		tm := s.RunningTasks()
		if numTypes, numTasks := len(tm), s.NumTasks(); numTypes != 1 || numTasks != i+1 {
			t.Errorf("stopper should have %d running tasks, got %d / %+v", i+1, numTasks, tm)
		}
		m := s.RunningTasks()
		if len(m) != 1 {
			t.Fatalf("expected exactly one task map entry: %+v", m)
		}
		for _, v := range m {
			if expNum := len(tasks); v != expNum {
				t.Fatalf("%d: expected %d tasks, got %d", i, expNum, v)
			}
		}
	}
	for i, c := range tasks {
		m := s.RunningTasks()
		if len(m) != 1 {
			t.Fatalf("%d: expected exactly one task map entry: %+v", i, m)
		}
		for _, v := range m {
			if expNum := len(tasks[i:]); v != expNum {
				t.Fatalf("%d: expected %d tasks, got %d:\n%s", i, expNum, v, m)
			}
		}
		// Close the channel to let the task proceed.
		close(c)
		expNum := len(tasks[i+1:])
		SucceedsSoon(t, func() error {
			if nt := s.NumTasks(); nt != expNum {
				return errors.Errorf("%d: stopper should have %d running tasks, got %d", i, expNum, nt)
			}
			return nil
		})
	}
	// The taskmap should've been cleared out.
	if m := s.RunningTasks(); len(m) != 0 {
		t.Fatalf("task map not empty: %+v", m)
	}
	s.Stop(context.Background())
}

// TestStopperRunTaskPanic ensures that a panic handler can recover panicking
// tasks, and that no tasks are leaked when they panic.
func TestStopperRunTaskPanic(t *testing.T) {
	ch := make(chan interface{})
	s := stop.NewStopper(stop.OnPanic(func(v interface{}) {
		ch <- v
	}))
	// If RunTask were not panic-safe, Stop() would deadlock.
	type testFn func()
	explode := func(context.Context) { panic(ch) }
	ctx := context.Background()
	for i, test := range []testFn{
		func() {
			_ = s.RunTask(ctx, explode)
		},
		func() {
			_ = s.RunAsyncTask(ctx, func(ctx context.Context) { explode(ctx) })
		},
		func() {
			_ = s.RunLimitedAsyncTask(
				context.Background(),
				make(chan struct{}, 1),
				true, /* wait */
				func(ctx context.Context) { explode(ctx) },
			)
		},
		func() {
			s.RunWorker(ctx, explode)
		},
	} {
		go test()
		recovered := <-ch
		if recovered != ch {
			t.Errorf("%d: unexpected recovered value: %+v", i, recovered)
		}
	}
}

func TestStopperWithCancel(t *testing.T) {
	s := stop.NewStopper()
	ctx := s.WithCancel(context.Background())
	s.Stop(ctx)
	if err := ctx.Err(); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestStopperShouldQuiesce(t *testing.T) {
	s := stop.NewStopper()
	running := make(chan struct{})
	runningTask := make(chan struct{})
	waiting := make(chan struct{})
	cleanup := make(chan struct{})
	ctx := context.Background()

	// Run a worker. A call to stopper.Stop(context.Background()) will not close until all workers
	// have completed, and this worker will complete when the "running" channel
	// is closed.
	s.RunWorker(ctx, func(context.Context) {
		<-running
	})
	// Run an asynchronous task. A stopper which has been Stop()ed will not
	// close it's ShouldStop() channel until all tasks have completed. This task
	// will complete when the "runningTask" channel is closed.
	if err := s.RunAsyncTask(ctx, func(_ context.Context) {
		<-runningTask
	}); err != nil {
		t.Fatal(err)
	}

	go func() {
		s.Stop(ctx)
		close(waiting)
		<-cleanup
	}()

	// The ShouldQuiesce() channel should close as soon as the stopper is
	// Stop()ed.
	<-s.ShouldQuiesce()
	// However, the ShouldStop() channel should still be blocked because the
	// async task started above is still running, meaning we haven't quiesceed
	// yet.
	select {
	case <-s.ShouldStop():
		t.Fatal("expected ShouldStop() to block until quiesceing complete")
	default:
		// Expected.
	}
	// After completing the running task, the ShouldStop() channel should
	// now close.
	close(runningTask)
	<-s.ShouldStop()
	// However, the working running above prevents the call to Stop() from
	// returning; it blocks until the runner's goroutine is finished. We
	// use the "waiting" channel to detect this.
	select {
	case <-waiting:
		t.Fatal("expected stopper to have blocked")
	default:
		// Expected.
	}
	// Finally, close the "running" channel, which should cause the original
	// call to Stop() to return.
	close(running)
	<-waiting
	close(cleanup)
}

func TestStopperRunLimitedAsyncTask(t *testing.T) {
	s := stop.NewStopper()
	defer s.Stop(context.Background())

	const maxConcurrency = 5
	const numTasks = maxConcurrency * 3
	sem := make(chan struct{}, maxConcurrency)
	taskSignal := make(chan struct{}, maxConcurrency)
	var mu sync.Mutex
	concurrency := 0
	peakConcurrency := 0
	var wg sync.WaitGroup

	f := func(_ context.Context) {
		mu.Lock()
		concurrency++
		if concurrency > peakConcurrency {
			peakConcurrency = concurrency
		}
		mu.Unlock()
		<-taskSignal
		mu.Lock()
		concurrency--
		mu.Unlock()
		wg.Done()
	}
	go func() {
		// Loop until the desired peak concurrency has been reached.
		for {
			mu.Lock()
			c := concurrency
			mu.Unlock()
			if c >= maxConcurrency {
				break
			}
			time.Sleep(time.Millisecond)
		}
		// Then let the rest of the async tasks finish quickly.
		for i := 0; i < numTasks; i++ {
			taskSignal <- struct{}{}
		}
	}()

	for i := 0; i < numTasks; i++ {
		wg.Add(1)
		if err := s.RunLimitedAsyncTask(
			context.Background(), sem, true /* wait */, f,
		); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if concurrency != 0 {
		t.Fatalf("expected 0 concurrency at end of test but got %d", concurrency)
	}
	if peakConcurrency != maxConcurrency {
		t.Fatalf("expected peak concurrency %d to equal max concurrency %d",
			peakConcurrency, maxConcurrency)
	}

	sem = make(chan struct{}, 1)
	sem <- struct{}{}
	err := s.RunLimitedAsyncTask(
		context.Background(), sem, false /* wait */, func(_ context.Context) {
		},
	)
	if err != stop.ErrThrottled {
		t.Fatalf("expected %v; got %v", stop.ErrThrottled, err)
	}
}

func TestStopperRunLimitedAsyncTaskCancelContext(t *testing.T) {
	s := stop.NewStopper()
	defer s.Stop(context.Background())

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)

	// Synchronization channels.
	workersDone := make(chan struct{})
	workerStarted := make(chan struct{})

	var workersRun int32
	var workersCancelled int32

	ctx, cancel := context.WithCancel(context.Background())
	f := func(ctx context.Context) {
		atomic.AddInt32(&workersRun, 1)
		workerStarted <- struct{}{}
		<-ctx.Done()
	}

	// This loop will block when the semaphore is filled.
	if err := s.RunAsyncTask(ctx, func(ctx context.Context) {
		for i := 0; i < maxConcurrency*2; i++ {
			if err := s.RunLimitedAsyncTask(ctx, sem, true, f); err != nil {
				if err != context.Canceled {
					t.Fatal(err)
				}
				atomic.AddInt32(&workersCancelled, 1)
			}
		}
		close(workersDone)
	}); err != nil {
		t.Fatal(err)
	}

	// Ensure that the semaphore fills up, leaving maxConcurrency workers
	// waiting for context cancelation.
	for i := 0; i < maxConcurrency; i++ {
		<-workerStarted
	}

	// Cancel the context, which should result in all subsequent attempts to
	// queue workers failing.
	cancel()
	<-workersDone

	if a, e := atomic.LoadInt32(&workersRun), int32(maxConcurrency); a != e {
		t.Fatalf("%d workers ran before context close, expected exactly %d", a, e)
	}
	if a, e := atomic.LoadInt32(&workersCancelled), int32(maxConcurrency); a != e {
		t.Fatalf("%d workers cancelled after context close, expected exactly %d", a, e)
	}
}

func maybePrint(context.Context) {
	if testing.Verbose() { // This just needs to be complicated enough not to inline.
		fmt.Println("blah")
	}
}

func BenchmarkDirectCall(b *testing.B) {
	s := stop.NewStopper()
	ctx := context.Background()
	defer s.Stop(ctx)
	for i := 0; i < b.N; i++ {
		maybePrint(ctx)
	}
}

func BenchmarkStopper(b *testing.B) {
	ctx := context.Background()
	s := stop.NewStopper()
	defer s.Stop(ctx)
	for i := 0; i < b.N; i++ {
		if err := s.RunTask(ctx, maybePrint); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkDirectCallPar(b *testing.B) {
	s := stop.NewStopper()
	ctx := context.Background()
	defer s.Stop(ctx)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			maybePrint(ctx)
		}
	})
}

func BenchmarkStopperPar(b *testing.B) {
	ctx := context.Background()
	s := stop.NewStopper()
	defer s.Stop(ctx)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := s.RunTask(ctx, maybePrint); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ===========================================================================
//
// The following helper functions are copied from cockroach's tree. This
// removes the last dependencies and allows the tests to run.
//
// DefaultSucceedsSoonDuration, SucceedsSoon() and SucceedsSoonDepth() copied
// verbatim from:
//  - github.com/cockroach/cockroachdb/pkg/util/retry.go
//
// RetryForDuration() copies verbatim from:
//  - github.com/cockroach/cockroachdb/pkg/testutils/soon.go
//
// ===========================================================================

// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

// DefaultSucceedsSoonDuration is the maximum amount of time unittests
// will wait for a condition to become true. See SucceedsSoon().
const DefaultSucceedsSoonDuration = 45 * time.Second

// SucceedsSoon fails the test (with t.Fatal) unless the supplied
// function runs without error within a preset maximum duration. The
// function is invoked immediately at first and then successively with
// an exponential backoff starting at 1ns and ending at the maximum
// duration (currently 15s).
func SucceedsSoon(t testing.TB, fn func() error) {
	SucceedsSoonDepth(1, t, fn)
}

// SucceedsSoonDepth is like SucceedsSoon() but with an additional
// stack depth offset.
func SucceedsSoonDepth(depth int, t testing.TB, fn func() error) {
	if err := RetryForDuration(DefaultSucceedsSoonDuration, fn); err != nil {
		file, line, _ := caller.Lookup(depth + 1)
		t.Fatalf("%s:%d, condition failed to evaluate within %s: %s", file, line, DefaultSucceedsSoonDuration, err)
	}
}

// RetryForDuration will retry the given function until it either returns
// without error, or the given duration has elapsed. The function is invoked
// immediately at first and then successively with an exponential backoff
// starting at 1ns and ending at the specified duration.
func RetryForDuration(duration time.Duration, fn func() error) error {
	deadline := time.Now().Add(duration)
	var lastErr error
	for wait := time.Duration(1); time.Now().Before(deadline); wait *= 2 {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if wait > time.Second {
			wait = time.Second
		}
		time.Sleep(wait)
	}
	return lastErr
}
