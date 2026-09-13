package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/coder/websocket"
)

const maxFrame = 64 << 20

// readJSONLine handles split writes and frames beyond bufio.Scanner's 64 KiB
// default, without allowing an unbounded allocation on malformed input.
func readJSONLine(r *bufio.Reader) ([]byte, error) {
	var b []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(b)+len(part) > maxFrame {
			return nil, errors.New("Codex protocol frame exceeds 64 MiB")
		}
		b = append(b, part...)
		if err == nil {
			return b, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(b) != 0 {
			return nil, errors.New("truncated Codex protocol frame")
		}
		return nil, err
	}
}

type packet struct {
	data   []byte
	source string
	err    error
}

func Bridge(ctx context.Context, ws *websocket.Conn, input io.Reader, output io.Writer, record *Record, save func(*Record) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ws.SetReadLimit(maxFrame)
	packets := make(chan packet, 16)
	sendPacket := func(p packet) {
		select {
		case packets <- p:
		case <-ctx.Done():
		}
	}
	go func() {
		r := bufio.NewReaderSize(input, 64<<10)
		for {
			b, err := readJSONLine(r)
			sendPacket(packet{data: b, source: "server", err: err})
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			kind, b, err := ws.Read(ctx)
			if err == nil && kind != websocket.MessageText {
				err = errors.New("Codex sent a non-text protocol frame")
			}
			sendPacket(packet{data: b, source: "TUI", err: err})
			if err != nil {
				return
			}
		}
	}()
	// Dedicated writers preserve message order and keep an unresponsive peer
	// from blocking timer cancellation or child-process shutdown.
	serverOut, tuiOut := make(chan []byte, 16), make(chan []byte, 16)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-serverOut:
				if _, err := output.Write(append(b, '\n')); err != nil {
					sendPacket(packet{source: "server writer", err: err})
					return
				}
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-tuiOut:
				writeCtx, stop := context.WithTimeout(ctx, 15*time.Second)
				err := ws.Write(writeCtx, websocket.MessageText, b)
				stop()
				if err != nil {
					sendPacket(packet{source: "TUI writer", err: err})
					return
				}
			}
		}
	}()
	queue := func(ch chan []byte) func([]byte) error {
		return func(b []byte) error {
			timeout := time.NewTimer(15 * time.Second)
			defer timeout.Stop()
			select {
			case ch <- b:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-timeout.C:
				return errors.New("Codex protocol peer is not draining its output queue")
			}
		}
	}
	e := NewEngine(record, queue(serverOut), queue(tuiOut), save)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			// time.Now handles suspend/resume; a ticker value can predate wakeup.
			if err := e.Tick(time.Now()); err != nil {
				return err
			}
		case p := <-packets:
			if p.err != nil {
				return fmt.Errorf("%s connection ended: %w", p.source, p.err)
			}
			var err error
			if p.source == "TUI" {
				err = e.TUI(p.data, time.Now())
			} else {
				err = e.Server(p.data, time.Now())
			}
			if err != nil {
				return err
			}
		}
	}
}
