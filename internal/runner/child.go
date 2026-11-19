package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// Options configures StartChild.
type Options struct {
	PTY   bool     // child's stdout and stderr share one pty; else two separate pipes
	Stdin *os.File // child's stdin, passed directly; nil means os.Stdin
	Env   []string // child's environment; nil means os.Environ()
}

// Child is a wrapped command. It is deliberately not given its own process
// group or session: it keeps loghorn's controlling terminal and foreground
// process group, so the terminal delivers Ctrl-C and job-control signals to it
// directly.
type Child struct {
	cmd    *exec.Cmd
	out    io.Reader
	err    io.Reader
	ptmx   *os.File
	closer []*os.File
	exited chan struct{}
	status int
}

// StartChild starts argv. A start failure wraps the exec error.
func StartChild(argv []string, o Options) (*Child, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command to run")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = o.Env
	if o.Stdin != nil {
		cmd.Stdin = o.Stdin
	} else {
		cmd.Stdin = os.Stdin
	}
	c := &Child{cmd: cmd, exited: make(chan struct{})}

	var childEnds []*os.File // closed in the parent once the child holds them
	fail := func(err error) (*Child, error) {
		for _, f := range childEnds {
			f.Close()
		}
		for _, f := range c.closer {
			f.Close()
		}
		return nil, err
	}

	if o.PTY {
		ptmx, pts, err := pty.Open()
		if err != nil {
			return nil, fmt.Errorf("allocating a pty for %s: %w", argv[0], err)
		}
		cmd.Stdout, cmd.Stderr = pts, pts
		childEnds = append(childEnds, pts)
		c.ptmx = ptmx
		c.closer = append(c.closer, ptmx)
		c.out = eofOnEIO{ptmx}
	} else {
		// Plain os.Pipe ends rather than cmd.StdoutPipe: Wait closes the
		// latter, which would cut off output still unread when the child exits.
		or, ow, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		c.closer = append(c.closer, or)
		childEnds = append(childEnds, ow)
		er, ew, err := os.Pipe()
		if err != nil {
			return fail(err)
		}
		c.closer = append(c.closer, er)
		childEnds = append(childEnds, ew)
		cmd.Stdout, cmd.Stderr = ow, ew
		c.out, c.err = or, er
	}

	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("starting %s: %w", argv[0], err))
	}
	for _, f := range childEnds {
		f.Close()
	}
	go func() {
		err := cmd.Wait()
		c.status = exitStatus(err)
		close(c.exited)
	}()
	return c, nil
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 1
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ws.ExitStatus()
	}
	return ee.ExitCode()
}

// eofOnEIO maps the EIO a pty master returns once the slave closes to io.EOF.
type eofOnEIO struct{ f *os.File }

func (r eofOnEIO) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if errors.Is(err, syscall.EIO) {
		err = io.EOF
	}
	return n, err
}

// Stdout is the pty master in PTY mode (both streams), else the stdout pipe.
func (c *Child) Stdout() io.Reader { return c.out }

// Stderr is nil in PTY mode.
func (c *Child) Stderr() io.Reader { return c.err }

// Pid is the child's process id.
func (c *Child) Pid() int { return c.cmd.Process.Pid }

// Exited is closed once the child has been reaped.
func (c *Child) Exited() <-chan struct{} { return c.exited }

// ExitStatus is the exit code, or 128+signal if killed by one. Valid after
// Exited is closed.
func (c *Child) ExitStatus() int { return c.status }

// Signal sends sig to the child process only. It returns nil once the child
// has exited.
func (c *Child) Signal(sig syscall.Signal) error {
	select {
	case <-c.exited:
		return nil
	default:
	}
	err := c.cmd.Process.Signal(sig)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

// InheritSize copies tty's window size to the pty. A no-op without PTY.
func (c *Child) InheritSize(tty *os.File) error {
	if c.ptmx == nil {
		return nil
	}
	return pty.InheritSize(tty, c.ptmx)
}

// Close closes loghorn's ends of the pty or pipes.
func (c *Child) Close() {
	for _, f := range c.closer {
		f.Close()
	}
}
