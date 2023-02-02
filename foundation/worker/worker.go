// Package worker manages a set of registered jobs that execute on demand.
package worker

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"sync"
	"time"
)

// JobFunc defines a function that can execute work for a specific job.
type JobFunc func(ctx context.Context)

// Worker manages jobs and the execution of those jobs concurrently.
type Worker struct {
	wg         sync.WaitGroup
	sem        chan bool
	isShutdown chan struct{}

	mu      sync.RWMutex
	running map[string]context.CancelFunc
}

// New constructs a Worker for managing and executing jobs. The capacity value
// represents the maximum number of G's that can be executing at any given time.
func New(capacity int) (*Worker, error) {
	if capacity < 0 {
		return nil, errors.New("max running jobs must be greater than 0")
	}

	sem := make(chan bool, capacity)
	for i := 0; i < capacity; i++ {
		sem <- true
	}

	w := Worker{
		sem:        sem,
		isShutdown: make(chan struct{}),
		running:    make(map[string]context.CancelFunc),
	}

	return &w, nil
}

// Running returns the number of jobs running.
func (w *Worker) Running() int {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return len(w.running)
}

// Shutdown waits for all jobs to complete before it returns
func (w *Worker) Shutdown(ctx context.Context) error {
	// Signal we are shutting down.
	close(w.isShutdown)

	// Call the cancel function for all running goroutines.
	w.mu.Lock()
	{
		for _, cancel := range w.running {
			cancel()
		}
	}
	w.mu.Unlock()

	// Launch a goroutine to wait for all the worker goroutines
	// to complete their work.
	ch := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(ch)
	}()

	// Wait for the goroutines to report they are done or when
	// the timeout is reached.
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start lookups a job by key and launches a goroutine to perform the work. A
// work key is returned so the caller can cancel work early.
func (w *Worker) Start(ctx context.Context, fn JobFunc) (string, error) {
	// We need to block here waiting to capture a semaphore, timeout or shutdown.
	// The shutdown is first to handle that event as priority.
	select {
	case <-w.isShutdown:
		return "", errors.New("shutting down")
	case <-ctx.Done():
		return "", ctx.Err()
	case <-w.sem:
	}

	// Need a unique key for this work.
	workKey := uuid.NewString()

	// Let's continue with the current context's deadline.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Second)
	}

	// Create a cancel function and keep it for stop/shutdown purposes.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)

	// Register this new G as running
	w.mu.Lock()
	defer w.mu.Unlock()
	w.running[workKey] = cancel

	// Launch a goroutine to perform the work.
	w.wg.Add(1)
	go func() {
		defer func() { w.sem <- true }()
		defer func() {
			cancel()
			w.removeWork(workKey)
			w.wg.Done()
		}()

		// Execute the actual workload.
		fn(ctx)
	}()

	return workKey, nil
}

// Stop is used to cancel an existing job that is running.
func (w *Worker) Stop(workKey string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	cancel, exists := w.running[workKey]
	if !exists {
		return fmt.Errorf("work[%s] is not running", workKey)
	}

	// Call cancel to stop the work.
	cancel()

	return nil
}

// removeWork removes the work from the running list.
func (w *Worker) removeWork(workKey string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.running, workKey)
}