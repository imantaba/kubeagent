package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/imantaba/kubeagent/internal/redact"
)

// escTimeout is how long the loop waits for the rest of an escape sequence
// before deciding a lone 0x1b was a real esc press. Terminals emit the bytes of
// an arrow key back-to-back, so this only has to outlast a scheduling hiccup;
// it is also the worst-case delay on esc, so it stays short.
const escTimeout = 50 * time.Millisecond

// Control sequences the terminal lifecycle needs. The alternate screen means
// quitting restores the scrollback the operator had before kubeagent ran.
const (
	escEnterAlt = "\x1b[?1049h"
	escExitAlt  = "\x1b[?1049l"
	escHideCurs = "\x1b[?25l"
	escShowCurs = "\x1b[?25h"
)

// Options configures a TUI session.
//
// Scan is injected rather than called here so main owns the scan.Options — the
// same split runGate uses — and so this package needs no Kubernetes client.
type Options struct {
	Version string
	Scope   string // "all namespaces" or "namespace shop"
	Scan    func(context.Context) (ScanSnapshot, error)
}

// checkTTY refuses to run without a terminal on both ends. isTerm is injected so
// the refusal is testable without a pty.
//
// The check runs before kubeagent connects to anything: a pipeline that reached
// for the TUI by mistake should fail immediately with advice, not after a scan.
// The message names no kubeconfig path and no context, because stdout here is
// whatever the operator redirected it to.
func checkTTY(inFD, outFD int, isTerm func(int) bool) error {
	if !isTerm(inFD) || !isTerm(outFD) {
		return errors.New("tui needs an interactive terminal; use 'kubeagent scan' for pipes and files")
	}
	return nil
}

// Run draws the TUI until the operator quits. It is the only function in this
// package that touches a terminal, a signal or the cluster; everything it draws
// comes from Render and everything it decides comes from Update.
func Run(ctx context.Context, opts Options) error {
	inFD, outFD := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if err := checkTTY(inFD, outFD, term.IsTerminal); err != nil {
		return err
	}

	// The first scan happens before raw mode, so a connection failure prints as
	// an ordinary error on an ordinary terminal.
	snap, err := opts.Scan(ctx)
	if err != nil {
		return err
	}

	width, height, err := term.GetSize(outFD)
	if err != nil {
		return fmt.Errorf("read terminal size: %w", err)
	}

	old, err := term.MakeRaw(inFD)
	if err != nil {
		return fmt.Errorf("enter raw mode: %w", err)
	}
	restore := func() {
		fmt.Fprint(os.Stdout, escShowCurs+escExitAlt)
		_ = term.Restore(inFD, old)
	}
	// One deferred function covers both exits. It restores first and re-panics
	// after, so a panic message prints to a cooked terminal instead of
	// staircasing across an alternate screen the operator cannot get out of.
	defer func() {
		restore()
		if r := recover(); r != nil {
			panic(r)
		}
	}()
	fmt.Fprint(os.Stdout, escEnterAlt+escHideCurs)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	// Exactly one goroutine, and it does nothing but move bytes: stdin has no
	// non-blocking read, so a read has to happen off the main loop for select to
	// work. The cluster is only ever touched from the main loop below.
	keys := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				keys <- b
			}
			if err != nil {
				close(keys)
				return
			}
		}
	}()

	m := Model{
		Version: opts.Version,
		Scope:   opts.Scope,
		Width:   width,
		Height:  height,
		Colour:  os.Getenv("NO_COLOR") == "",
	}
	m = Update(m, Event{Kind: EventScanned, Result: &snap})
	draw(m)

	var pending []byte
	for !m.Quit {
		var timeout <-chan time.Time
		if len(pending) > 0 {
			timeout = time.After(escTimeout)
		}
		select {
		case <-ctx.Done():
			return nil
		case b, ok := <-keys:
			if !ok {
				return nil // stdin closed
			}
			pending = append(pending, b...)
			pending, m = drainKeys(pending, m, false)
		case <-timeout:
			pending, m = drainKeys(pending, m, true)
		case sig := <-sigs:
			if sig != syscall.SIGWINCH {
				// SIGTERM or SIGHUP. Returning runs the deferred restore, which
				// is the whole reason these are handled rather than left to kill
				// the process with the terminal still in raw mode.
				return nil
			}
			if w, h, err := term.GetSize(outFD); err == nil {
				m = Update(m, Event{Kind: EventResize, Width: w, Height: h})
			}
		}
		if m.Quit {
			break
		}
		if m.Scanning {
			draw(m) // the busy frame, before the call blocks
			if snap, err := opts.Scan(ctx); err != nil {
				m = Update(m, Event{Kind: EventScanned, Err: redact.Error(err)})
			} else {
				m = Update(m, Event{Kind: EventScanned, Result: &snap})
			}
		}
		draw(m)
	}
	return nil
}

func draw(m Model) {
	fmt.Fprint(os.Stdout, Render(m))
}
