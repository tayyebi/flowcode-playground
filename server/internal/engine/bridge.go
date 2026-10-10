// The unix-socket bridge: fcplay's app plugins call back into the Go server
// here, and the apps service executes the real side effect. The core stays
// pure compute — it listens to nothing; this server is owned by the engine
// and lives exactly as long as the run's temp directory.
package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/apps"
)

// bridgeMaxCalls bounds how many app calls one run may make. The language has
// no real loops today, so this only guards against pathological bytecode.
const bridgeMaxCalls = 256

// bridgeRequest is one frame from the runner.
type bridgeRequest struct {
	name   string
	params map[string]string
	token  []byte
}

// bridgeSocketPath is where the listener goes for a run directory.
func bridgeSocketPath(runDir string) string {
	return filepath.Join(runDir, "bridge.sock")
}

// serveBridge accepts connections on ln (the runner connects once, lazily on
// its first app call) and answers frames until the run context ends. Every
// completed call is appended to res.AppCalls for the response page.
func (e *Engine) serveBridge(ctx context.Context, ln net.Listener, actor apps.Actor, res *Result) {
	var mu sync.Mutex

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed: the run is over (normal path — the deferred
			// close in compileAndRun runs when fcplay has exited).
			return
		}
		e.handleBridgeConn(ctx, conn, actor, res, &mu)
		conn.Close()
	}
}

func (e *Engine) handleBridgeConn(ctx context.Context, conn net.Conn, actor apps.Actor, res *Result, mu *sync.Mutex) {
	calls := 0
	for {
		req, err := readBridgeRequest(conn)
		if err != nil {
			return // EOF or malformed frame: the runner is done or died
		}
		calls++
		if calls > bridgeMaxCalls {
			writeBridgeResponse(conn, false, "error: too many app calls in one run", "")
			return
		}

		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		start := time.Now()
		outcome, err := e.Apps.Call(callCtx, actor, req.name, req.params, req.token)
		elapsed := time.Since(start).Milliseconds()
		cancel()

		ac := AppCall{
			Name:       req.name,
			Request:    outcome.Request,
			Response:   outcome.Response,
			DurationMs: elapsed,
		}
		if err != nil {
			ac.Status = outcome.Status // "error: …"
			writeBridgeResponse(conn, false, ac.Status, "")
		} else {
			ac.Status = outcome.Status
			writeBridgeResponse(conn, true, outcome.Status, outcome.Result)
		}

		mu.Lock()
		res.AppCalls = append(res.AppCalls, &ac)
		mu.Unlock()
	}
}

/* ------------------------------------------------------------------ */
/* Wire format (little-endian lengths, no JSON in C)                   */
/* ------------------------------------------------------------------ */
//
// Request:  u32 name_len | name | u32 param_count | (u32 klen | k | u32 vlen | v)* | u32 token_len | token
// Response: u8 ok | u32 status_len | status | u32 result_len | result

func readBridgeRequest(r io.Reader) (*bridgeRequest, error) {
	var len32 uint32
	name, err := readBridgeString(r, &len32)
	if err != nil {
		return nil, err
	}
	if len(name) == 0 || len(name) > 128 {
		return nil, fmt.Errorf("bad app name length")
	}
	if err := binary.Read(r, binary.LittleEndian, &len32); err != nil {
		return nil, err
	}
	if len32 > 64 {
		return nil, fmt.Errorf("bad param count %d", len32)
	}
	req := &bridgeRequest{name: string(name), params: map[string]string{}}
	for i := uint32(0); i < len32; i++ {
		k, err := readBridgeString(r, nil)
		if err != nil {
			return nil, err
		}
		v, err := readBridgeString(r, nil)
		if err != nil {
			return nil, err
		}
		req.params[string(k)] = string(v)
	}
	token, err := readBridgeString(r, nil)
	if err != nil {
		return nil, err
	}
	req.token = token
	return req, nil
}

// readBridgeString reads a u32 length followed by that many bytes. When
// lenOut is non-nil it receives the raw length (so callers can reject
// unreasonable counts before allocating).
func readBridgeString(r io.Reader, lenOut *uint32) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	if lenOut != nil {
		*lenOut = n
	}
	if n > 1<<20 {
		return nil, errors.New("bridge frame too large")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeBridgeResponse(w io.Writer, ok bool, status, result string) {
	var b []byte
	if ok {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	b = bridgeAppendString(b, status)
	b = bridgeAppendString(b, result)
	w.Write(b) //nolint: the runner dies on read error; nothing to recover
}

func bridgeAppendString(b []byte, s string) []byte {
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(s)))
	b = append(b, n[:]...)
	return append(b, s...)
}
