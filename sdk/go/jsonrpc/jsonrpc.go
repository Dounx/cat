// Package jsonrpc implements the wire format Cat and its plugins speak:
// JSON-RPC 2.0, one message per line, in both directions over a byte stream.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// CancelMethod is a notification asking the peer to abandon a request.
const CancelMethod = "$/cancel"

const (
	CodeMethodNotFound = -32601
	CodeInternal       = -32603
	// CodeCancelled is returned when the user cancels an interactive step.
	CodeCancelled = -32800
)

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// ErrClosed is returned by calls made on, or interrupted by, a closed connection.
var ErrClosed = errors.New("jsonrpc: connection closed")

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

// Handler serves one method. Its result is marshalled as the response.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

type Conn struct {
	r *bufio.Reader
	w io.Writer

	wmu sync.Mutex
	mu  sync.Mutex

	handlers map[string]Handler
	pending  map[string]chan message
	inflight map[string]context.CancelFunc
	nextID   int64
	closed   chan struct{}
	once     sync.Once
}

func New(r io.Reader, w io.Writer) *Conn {
	return &Conn{
		r: bufio.NewReaderSize(r, 64<<10), w: w,
		handlers: map[string]Handler{},
		pending:  map[string]chan message{},
		inflight: map[string]context.CancelFunc{},
		closed:   make(chan struct{}),
	}
}

// Handle registers a method handler. Call before Serve.
func (c *Conn) Handle(method string, h Handler) { c.handlers[method] = h }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.closed }

func (c *Conn) shutdown() {
	c.once.Do(func() {
		close(c.closed)
		c.mu.Lock()
		for _, cancel := range c.inflight {
			cancel()
		}
		c.mu.Unlock()
	})
}

// Close marks the connection ended; pending calls return ErrClosed.
func (c *Conn) Close() { c.shutdown() }

// Serve reads messages until the stream ends. It always returns a non-nil
// error; io.EOF means the peer closed cleanly.
func (c *Conn) Serve() error {
	defer c.shutdown()
	for {
		line, err := c.r.ReadBytes('\n')
		if len(line) > 1 {
			var m message
			if json.Unmarshal(line, &m) == nil {
				c.dispatch(m)
			}
		}
		if err != nil {
			return err
		}
	}
}

func (c *Conn) dispatch(m message) {
	if m.Method == "" { // a response
		if m.ID == nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[string(*m.ID)]
		delete(c.pending, string(*m.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
		return
	}
	if m.Method == CancelMethod {
		var p struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		cancel := c.inflight[string(p.ID)]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	h := c.handlers[m.Method]
	if m.ID == nil { // a notification
		if h != nil {
			go func() { _, _ = h(context.Background(), m.Params) }()
		}
		return
	}
	id := *m.ID
	if h == nil {
		c.write(message{ID: &id, Error: &Error{Code: CodeMethodNotFound, Message: "method not found: " + m.Method}})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.inflight[string(id)] = cancel
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.inflight, string(id))
			c.mu.Unlock()
			cancel()
		}()
		resp := message{ID: &id}
		result, err := safely(ctx, h, m.Params)
		if err != nil {
			var rpcErr *Error
			if errors.As(err, &rpcErr) {
				resp.Error = rpcErr
			} else {
				resp.Error = &Error{Code: CodeInternal, Message: err.Error()}
			}
			// A failed call may still carry data (e.g. partial output).
			if result != nil && resp.Error.Data == nil {
				resp.Error.Data, _ = json.Marshal(result)
			}
		} else {
			b, merr := json.Marshal(result)
			if merr != nil {
				resp.Error = &Error{Code: CodeInternal, Message: merr.Error()}
			} else {
				resp.Result = b
			}
		}
		c.write(resp)
	}()
}

func safely(ctx context.Context, h Handler, params json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, params)
}

func (c *Conn) write(m message) error {
	m.JSONRPC = "2.0"
	if m.ID != nil && m.Method == "" && m.Error == nil && m.Result == nil {
		m.Result = json.RawMessage("null")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

func raw(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// Notify sends a notification: no response is expected.
func (c *Conn) Notify(method string, params any) error {
	p, err := raw(params)
	if err != nil {
		return err
	}
	return c.write(message{Method: method, Params: p})
}

// Call sends a request and waits for its response, decoding it into result
// (which may be nil). Cancelling ctx tells the peer to abandon the request.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	p, err := raw(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.nextID++
	id := json.RawMessage(fmt.Sprint(c.nextID))
	ch := make(chan message, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
	}
	if err := c.write(message{ID: &id, Method: method, Params: p}); err != nil {
		forget()
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		forget()
		_ = c.Notify(CancelMethod, map[string]any{"id": id})
		return ctx.Err()
	case <-c.closed:
		forget()
		return ErrClosed
	}
}
