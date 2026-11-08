package isolation

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// closeGrace is how long Close waits for a process to exit on its own after
// its stdin closes, before it is cancelled.
const closeGrace = 5 * time.Second

// Process is a long-lived command in an instance, spoken to over its stdin
// and stdout: how a stdio tool server runs in a run's instance. Its stderr
// is discarded.
type Process struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	err  error // what Exec returned
	code int
	once sync.Once
}

// Start runs cmd in inst and returns it while it runs. The process outlives
// ctx's cancellation (it is a server, not a call); Close ends it, and so
// does closing the instance.
func Start(ctx context.Context, inst Instance, cmd Command) (*Process, error) {
	if inst == nil {
		return nil, ErrUnisolated
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ectx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &Process{stdin: inW, stdout: outR, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		st, err := inst.Exec(ectx, cmd, Stdio{Stdin: inR, Stdout: outW})
		p.mu.Lock()
		p.err, p.code = err, st.Code
		p.mu.Unlock()
		// The reader sees the end of the stream, or why it ended.
		if err == nil {
			err = io.EOF
		}
		_ = outW.CloseWithError(err)
		_ = inR.CloseWithError(err)
	}()
	return p, nil
}

// Read reads the process's stdout.
func (p *Process) Read(b []byte) (int, error) { return p.stdout.Read(b) }

// Write writes to the process's stdin.
func (p *Process) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Close closes the process's stdin, waits a moment for it to exit, then
// cancels it. It returns once the process has ended.
func (p *Process) Close() error {
	p.once.Do(func() {
		_ = p.stdin.Close()
		t := time.NewTimer(closeGrace)
		defer t.Stop()
		select {
		case <-p.done:
		case <-t.C:
			p.cancel()
			<-p.done
		}
		p.cancel()
		_ = p.stdout.Close()
	})
	return nil
}

// Done is closed when the process has ended.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err is what ended the process, once Done is closed: nil for an exit, the
// exit code in ExitCode.
func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if errors.Is(p.err, context.Canceled) {
		return nil
	}
	return p.err
}

// ExitCode is the process's exit code once Done is closed.
func (p *Process) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}
