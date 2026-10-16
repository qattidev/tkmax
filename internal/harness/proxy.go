package harness

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Codex's proxy carries raw Unix-socket bytes, including the HTTP Upgrade and
// WebSocket frames. It is NOT the JSONL transport offered by app-server --stdio.
type proxyTransport struct {
	Input  io.ReadCloser
	Output io.WriteCloser
	child  *child
	close  func()
}

func startProxy(ctx context.Context, cmd *exec.Cmd) (*proxyTransport, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	c, err := startChild(cmd, true)
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	conn := &stdioConn{Reader: stdout, Writer: stdin}
	used := false
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		if used {
			return nil, errors.New("Codex proxy connection already consumed")
		}
		used = true
		return conn, nil
	}}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ws, _, err := websocket.Dial(dialCtx, "ws://codex-local/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	cancel()
	if err != nil {
		conn.Close()
		c.stop()
		return nil, err
	}
	ws.SetReadLimit(maxFrame)
	streamCtx, stop := context.WithCancel(ctx)
	in, received := io.Pipe()
	sent, out := io.Pipe()
	var once sync.Once
	p := &proxyTransport{Input: in, Output: out, child: c}
	p.close = func() {
		once.Do(func() {
			stop()
			ws.CloseNow()
			in.Close()
			received.Close()
			sent.Close()
			out.Close()
			conn.Close()
			transport.CloseIdleConnections()
			c.stop()
		})
	}
	go func() {
		for {
			kind, b, err := ws.Read(streamCtx)
			if err == nil && kind != websocket.MessageText {
				err = errors.New("Codex proxy returned a non-text frame")
			}
			if err != nil {
				received.CloseWithError(err)
				return
			}
			if _, err := received.Write(append(b, '\n')); err != nil {
				return
			}
		}
	}()
	go func() {
		reader := bufio.NewReader(sent)
		for {
			b, err := readJSONLine(reader)
			if err != nil {
				sent.CloseWithError(err)
				return
			}
			if err := ws.Write(streamCtx, websocket.MessageText, b); err != nil {
				sent.CloseWithError(err)
				return
			}
		}
	}()
	return p, nil
}

func (p *proxyTransport) Close() { p.close() }

type stdioConn struct {
	Reader io.ReadCloser
	Writer io.WriteCloser
}

func (c *stdioConn) Read(b []byte) (int, error)  { return c.Reader.Read(b) }
func (c *stdioConn) Write(b []byte) (int, error) { return c.Writer.Write(b) }
func (c *stdioConn) Close() error                { return errors.Join(c.Reader.Close(), c.Writer.Close()) }
func (*stdioConn) LocalAddr() net.Addr           { return stdioAddr("tkmax") }
func (*stdioConn) RemoteAddr() net.Addr          { return stdioAddr("codex-proxy") }
func (c *stdioConn) SetReadDeadline(t time.Time) error {
	if d, ok := c.Reader.(interface{ SetReadDeadline(time.Time) error }); ok {
		return d.SetReadDeadline(t)
	}
	return nil
}
func (c *stdioConn) SetWriteDeadline(t time.Time) error {
	if d, ok := c.Writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return d.SetWriteDeadline(t)
	}
	return nil
}
func (c *stdioConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

type stdioAddr string

func (stdioAddr) Network() string  { return "stdio" }
func (a stdioAddr) String() string { return string(a) }
