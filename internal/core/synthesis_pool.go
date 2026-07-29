package internal

import (
	"context"
	"sync"
	"time"

	"github.com/flowbyte-com/mpm-core/synth"
)

// SynthesisTask represents a single synthesis job.
type SynthesisTask struct {
	MemoryID string
	Content  string
	DM       CoreDB
	Client   *synth.SynthClient
	Ctx      context.Context
}

// SynthesisPool is a bounded worker pool for synthesis tasks.
type SynthesisPool struct {
	tasks     chan SynthesisTask
	workers   int
	wg        sync.WaitGroup
	mu        sync.Mutex
	started   bool
	shutdown  bool
	shutdownC chan struct{}
}

// GetSynthesisPool returns a singleton synthesis pool with the given max workers.
// Safe for concurrent use.
func GetSynthesisPool(maxWorkers int) *SynthesisPool {
	synthesisPoolOnce.Do(func() {
		synthesisPool = &SynthesisPool{
			tasks:     make(chan SynthesisTask, maxWorkers*2),
			workers:   maxWorkers,
			shutdownC: make(chan struct{}),
		}
	})
	// Ensure workers are started
	synthesisPool.startWorkers()
	return synthesisPool
}

var (
	synthesisPool     *SynthesisPool
	synthesisPoolOnce sync.Once
)

func (p *SynthesisPool) startWorkers() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return
	}
	p.started = true
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
}

func (p *SynthesisPool) worker(id int) {
	defer p.wg.Done()
	for {
		select {
		case task, ok := <-p.tasks:
			if !ok {
				return
			}
			// Execute synthesis with the task's context
			AutoSynthesize(task.Ctx, task.DM, task.Client, task.MemoryID, task.Content)
		case <-p.shutdownC:
			return
		}
	}
}

// Submit adds a synthesis task to the pool.
// Returns ErrSynthesisPoolFull if the queue is full (non-blocking).
func (p *SynthesisPool) Submit(ctx context.Context, dm CoreDB, client *synth.SynthClient, memoryID, content string) error {
	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return ErrSynthesisPoolFull{}
	}
	p.mu.Unlock()

	select {
	case p.tasks <- SynthesisTask{
		MemoryID: memoryID,
		Content:  content,
		DM:       dm,
		Client:   client,
		Ctx:      ctx,
	}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrSynthesisPoolFull{}
	}
}

// Shutdown gracefully stops the pool, waiting for in-flight tasks.
func (p *SynthesisPool) Shutdown(timeout time.Duration) error {
	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return nil
	}
	p.shutdown = true
	close(p.shutdownC)
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return ErrSynthesisShutdownTimeout{}
	}
}

// ErrSynthesisPoolFull is returned when the synthesis worker pool queue is full.
type ErrSynthesisPoolFull struct{}

func (ErrSynthesisPoolFull) Error() string {
	return "synthesis worker pool queue full, task dropped"
}

// ErrSynthesisShutdownTimeout is returned when shutdown exceeds timeout.
type ErrSynthesisShutdownTimeout struct{}

func (ErrSynthesisShutdownTimeout) Error() string {
	return "synthesis worker pool shutdown timeout"
}