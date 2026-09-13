package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestJSONLineFragmentationAndSize(t *testing.T) {
	body := `{"method":"item/agentMessage/delta","params":{"delta":"` + strings.Repeat("x", 200000) + `"}}` + "\n"
	r := bufio.NewReaderSize(strings.NewReader(body+body), 32)
	for i := 0; i < 2; i++ {
		b, err := readJSONLine(r)
		if err != nil || string(b) != body {
			t.Fatalf("large fragmented frame: %v", err)
		}
	}
	if _, err := readJSONLine(r); err != io.EOF {
		t.Fatalf("expected EOF: %v", err)
	}
	if _, err := readJSONLine(bufio.NewReader(strings.NewReader(`{"incomplete":true}`))); err == nil {
		t.Fatal("accepted unterminated frame")
	}
}

func TestBridgeWebSocketToJSONLAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backendIn, serverOut := io.Pipe()
	defer backendIn.Close()
	defer serverOut.Close()
	serverIn, backendOut := io.Pipe()
	defer serverIn.Close()
	defer backendOut.Close()
	done := make(chan error, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer ws.CloseNow()
		record := &Record{FallbackWait: time.Hour}
		done <- Bridge(ctx, ws, backendIn, backendOut, record, func(*Record) error { return nil })
	}))
	defer httpServer.Close()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	client.SetReadLimit(maxFrame)
	request := map[string]any{"id": 3, "method": "future/echo", "params": map[string]any{"data": strings.Repeat("z", 200000)}}
	if err := client.Write(ctx, websocket.MessageText, raw(request)); err != nil {
		t.Fatal(err)
	}
	line, err := readJSONLine(bufio.NewReader(serverIn))
	if err != nil {
		t.Fatal(err)
	}
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["id"]) == "3" {
		t.Fatal("ID not mapped")
	}
	go func() {
		_, _ = serverOut.Write(append(raw(map[string]any{"id": m["id"], "result": m["params"]}), '\n'))
	}()
	readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	_, reply, err := client.Read(readCtx)
	if err != nil {
		t.Fatal(err)
	}
	var response message
	_ = json.Unmarshal(reply, &response)
	if string(response["id"]) != "3" || string(response["result"]) != string(m["params"]) {
		t.Fatal("bridge corrupted reply")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bridge did not cancel")
	}
}
