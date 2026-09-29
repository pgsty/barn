// Package qmp implements the minimal short-connection QEMU Machine Protocol
// client required by Barn lifecycle and identity checks.
package qmp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

const (
	defaultTimeout = 5 * time.Second
	defaultMaxRead = 1 << 20
)

type Error struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *Error) Error() string { return fmt.Sprintf("QMP %s: %s", e.Class, e.Desc) }

type Event struct {
	Name      string          `json:"event"`
	Data      json.RawMessage `json:"data,omitempty"`
	Timestamp json.RawMessage `json:"timestamp,omitempty"`
}

type Client struct {
	Timeout time.Duration
	MaxRead int64
	OnEvent func(Event)
	nextID  atomic.Uint64
}

type request struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
	ID        string `json:"id"`
}

type message struct {
	QMP       json.RawMessage `json:"QMP,omitempty"`
	Return    json.RawMessage `json:"return,omitempty"`
	Error     *Error          `json:"error,omitempty"`
	Event     string          `json:"event,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Timestamp json.RawMessage `json:"timestamp,omitempty"`
	ID        string          `json:"id,omitempty"`
}

func (c *Client) timeout() time.Duration {
	if c.Timeout <= 0 {
		return defaultTimeout
	}
	return c.Timeout
}

func (c *Client) maxRead() int64 {
	if c.MaxRead <= 0 {
		return defaultMaxRead
	}
	return c.MaxRead
}

// Execute opens a new Unix connection, negotiates capabilities, runs one
// command, demultiplexes interleaved events, and closes the connection.
func (c *Client) Execute(ctx context.Context, socket, command string, arguments any, result any) (returnErr error) {
	if socket == "" || command == "" {
		return errors.New("QMP socket and command must be non-empty")
	}
	dialer := net.Dialer{Timeout: c.timeout()}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("dial QMP socket %s: %w", socket, err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close QMP connection: %w", err))
		}
	}()

	deadline := time.Now().Add(c.timeout())
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set QMP deadline: %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancel()

	decoder := json.NewDecoder(io.LimitReader(conn, c.maxRead()))
	encoder := json.NewEncoder(conn)
	var greeting message
	if err := decoder.Decode(&greeting); err != nil {
		return fmt.Errorf("decode QMP greeting: %w", err)
	}
	if len(greeting.QMP) == 0 {
		return errors.New("QMP greeting does not contain QMP capabilities metadata")
	}
	if err := c.execute(ctx, decoder, encoder, "qmp_capabilities", nil, nil); err != nil {
		return fmt.Errorf("negotiate QMP capabilities: %w", err)
	}
	if err := c.execute(ctx, decoder, encoder, command, arguments, result); err != nil {
		return fmt.Errorf("execute QMP %s: %w", command, err)
	}
	return nil
}

func (c *Client) execute(ctx context.Context, decoder *json.Decoder, encoder *json.Encoder, command string, arguments, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := fmt.Sprintf("barn-%d", c.nextID.Add(1))
	if err := encoder.Encode(request{Execute: command, Arguments: arguments, ID: id}); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var response message
		if err := decoder.Decode(&response); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		if response.Event != "" {
			if c.OnEvent != nil {
				c.OnEvent(Event{Name: response.Event, Data: response.Data, Timestamp: response.Timestamp})
			}
			continue
		}
		if response.ID != id {
			continue
		}
		if response.Error != nil {
			return response.Error
		}
		if response.Return == nil {
			return errors.New("matched QMP response has neither return nor error")
		}
		if result == nil || string(response.Return) == "{}" || string(response.Return) == "null" {
			return nil
		}
		if err := json.Unmarshal(response.Return, result); err != nil {
			return fmt.Errorf("decode QMP return value: %w", err)
		}
		return nil
	}
}

type Status struct {
	Running    bool   `json:"running"`
	Singlestep bool   `json:"singlestep"`
	Status     string `json:"status"`
}

type Name struct {
	Name string `json:"name"`
}

type UUID struct {
	UUID string `json:"UUID"`
}

func (c *Client) QueryStatus(ctx context.Context, socket string) (Status, error) {
	var status Status
	err := c.Execute(ctx, socket, "query-status", nil, &status)
	return status, err
}

func (c *Client) QueryName(ctx context.Context, socket string) (Name, error) {
	var name Name
	err := c.Execute(ctx, socket, "query-name", nil, &name)
	return name, err
}

func (c *Client) QueryUUID(ctx context.Context, socket string) (UUID, error) {
	var uuid UUID
	err := c.Execute(ctx, socket, "query-uuid", nil, &uuid)
	return uuid, err
}

func (c *Client) Powerdown(ctx context.Context, socket string) error {
	return c.Execute(ctx, socket, "system_powerdown", nil, nil)
}

func (c *Client) Quit(ctx context.Context, socket string) error {
	return c.Execute(ctx, socket, "quit", nil, nil)
}
