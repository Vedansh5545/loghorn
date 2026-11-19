// Package wrap copies a wrapped child's output to loghorn's own stdout and
// stderr unchanged while recording every framed record, or with Filter prints
// only the important records. It forwards loghorn's signals to the child and
// returns the child's exit status.
package wrap

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/recorder"
	"github.com/jimbeveridge/loghorn/internal/runner"
)

// DefaultDrain is how long output readers get after the child is reaped when
// Options.Drain is zero.
const DefaultDrain = 500 * time.Millisecond

// Options configures Run and Pipe.
type Options struct {
	Recorder       *recorder.Recorder                             // required
	Frame          func(r io.Reader, emit func(rec []byte)) error // framing of the recorded copy; nil = ingest.Records(r, config.FormatAuto, emit)
	StripANSI      bool                                           // strip ANSI escape sequences from each framed record before recording
	Filter         bool                                           // print only important records (raw + "\n") to Stdout instead of passing bytes through
	Stdout, Stderr io.Writer
	Grace          time.Duration    // after a forwarded SIGTERM, SIGKILL the child this much later
	Signals        <-chan os.Signal // from NotifySignals; nil = no forwarding (tests)
	Terminal       *os.File         // a PTY child's window-size source; nil = none
	Drain          time.Duration    // how long output readers get after the child is reaped; 0 = DefaultDrain
}

// NotifySignals routes the signals Run forwards or drops to a channel. Using
// signal.Notify rather than signal.Ignore for SIGINT and SIGQUIT matters: an
// ignored signal stays ignored in the child across exec, while a handled one
// is reset to its default there. Call it before starting the child.
func NotifySignals() (sigs <-chan os.Signal, stop func()) {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch) }
}

// Run copies and records c's output, forwards signals, and returns c's exit
// status once c has been reaped and its output drained (at most o.Drain). It
// closes neither the recorder nor c.
func Run(c *runner.Child, o Options) int {
	o = withDefaults(o)
	if o.Terminal != nil {
		c.InheritSize(o.Terminal)
	}

	out := newOutput(o)
	var wg sync.WaitGroup
	copyStream := func(r io.Reader, dst io.Writer) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out.copy(eofReader{r}, dst)
		}()
	}
	copyStream(c.Stdout(), o.Stdout)
	if c.Stderr() != nil {
		copyStream(c.Stderr(), o.Stderr)
	}
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()

	var kill *time.Timer
wait:
	for {
		select {
		case <-c.Exited():
			break wait
		case sig := <-o.Signals:
			switch sig {
			case syscall.SIGTERM:
				c.Signal(syscall.SIGTERM)
				if kill == nil {
					kill = time.AfterFunc(o.Grace, func() { c.Signal(syscall.SIGKILL) })
				}
			case syscall.SIGHUP:
				c.Signal(syscall.SIGHUP)
			case syscall.SIGWINCH:
				if o.Terminal != nil {
					c.InheritSize(o.Terminal)
				}
			}
			// SIGINT and SIGQUIT are dropped: the terminal delivers them to
			// the child directly, and loghorn outlives it to drain and record.
		}
	}
	if kill != nil {
		kill.Stop()
	}

	// A grandchild can hold the output open long after the child exits; its
	// readers are abandoned after the drain.
	t := time.NewTimer(o.Drain)
	defer t.Stop()
	select {
	case <-drained:
	case <-t.C:
	}
	return c.ExitStatus()
}

// Pipe is Run for a stream with no child: r is copied to Stdout (or filtered)
// and recorded. It returns the framer's error; a write failure on Stdout is
// not reported, as with a child.
func Pipe(r io.Reader, o Options) error {
	o = withDefaults(o)
	return newOutput(o).copy(r, o.Stdout)
}

func withDefaults(o Options) Options {
	if o.Frame == nil {
		o.Frame = func(r io.Reader, emit func([]byte)) error {
			return ingest.Records(r, config.FormatAuto, emit)
		}
	}
	if o.Drain == 0 {
		o.Drain = DefaultDrain
	}
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	return o
}

// output is shared by a child's streams: it records each framed record and,
// with Filter, prints important ones to Stdout under one lock.
type output struct {
	o      Options
	mu     sync.Mutex // serialises filtered writes from both streams
	stdout *passWriter
}

func newOutput(o Options) *output {
	return &output{o: o, stdout: &passWriter{w: o.Stdout}}
}

// copy frames r, recording each record. Without Filter the bytes are teed to
// dst unchanged as they are read.
func (p *output) copy(r io.Reader, dst io.Writer) error {
	if !p.o.Filter {
		r = io.TeeReader(r, &passWriter{w: dst})
	}
	return p.o.Frame(r, p.emit)
}

func (p *output) emit(rec []byte) {
	if p.o.StripANSI {
		rec = stripANSI(rec)
	}
	e := p.o.Recorder.Line(rec)
	if !p.o.Filter || !e.Important {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	line := make([]byte, 0, len(e.Raw)+1)
	line = append(append(line, e.Raw...), '\n')
	p.stdout.Write(line)
}

// passWriter forwards bytes unchanged until the first write error, then
// swallows every later write, so recording continues and the child never
// blocks on a full pipe.
type passWriter struct {
	w      io.Writer
	failed bool
}

func (w *passWriter) Write(p []byte) (int, error) {
	if !w.failed {
		if _, err := w.w.Write(p); err != nil {
			w.failed = true
		}
	}
	return len(p), nil
}

// eofReader ends the stream cleanly when loghorn's end of it is closed or the
// pty reports EIO, so the framer still emits a final unterminated record.
type eofReader struct{ r io.Reader }

func (e eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && (errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EIO)) {
		err = io.EOF
	}
	return n, err
}

// stripANSI removes 7-bit escape sequences (CSI, OSC, DCS/SOS/PM/APC strings
// and two-byte escapes) and keeps every other byte, including invalid UTF-8,
// unchanged. An unterminated sequence is dropped to the end of rec.
func stripANSI(rec []byte) []byte {
	i := bytes.IndexByte(rec, 0x1b)
	if i < 0 {
		return rec
	}
	out := append(make([]byte, 0, len(rec)), rec[:i]...)
	for i < len(rec) {
		if rec[i] != 0x1b {
			out = append(out, rec[i])
			i++
			continue
		}
		i++ // ESC
		if i >= len(rec) {
			break
		}
		switch c := rec[i]; {
		case c == '[': // CSI: parameters and intermediates, then a final byte
			i++
			for i < len(rec) && (rec[i] < 0x40 || rec[i] > 0x7e) {
				i++
			}
			i++
		case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
			// A string, ended by BEL (OSC only, by convention) or ST (ESC \).
			i++
			for i < len(rec) {
				if rec[i] == 0x07 {
					i++
					break
				}
				if rec[i] == 0x1b && i+1 < len(rec) && rec[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
		default: // intermediates, then one final byte
			for i < len(rec) && rec[i] >= 0x20 && rec[i] <= 0x2f {
				i++
			}
			i++
		}
	}
	return out
}
