package qmp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func startServer(t *testing.T, handler func(*json.Decoder, *json.Encoder) error) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "barn-qmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "qmp.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		handlerErr := handler(json.NewDecoder(conn), json.NewEncoder(conn))
		done <- errors.Join(handlerErr, conn.Close())
	}()
	t.Cleanup(func() {
		select {
		case serverErr := <-done:
			if serverErr != nil {
				t.Errorf("fake QMP server: %v", serverErr)
			}
		case <-time.After(2 * time.Second):
			t.Error("fake QMP server did not finish")
		}
	})
	return socket
}

func TestQueryStatusDemultiplexesEvents(t *testing.T) {
	socket := startServer(t, func(decoder *json.Decoder, encoder *json.Encoder) error {
		if err := encoder.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{"qemu": map[string]int{"major": 10, "minor": 1, "micro": 0}}, "capabilities": []any{}}}); err != nil {
			return err
		}
		var request map[string]any
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		if request["execute"] != "qmp_capabilities" {
			return fmt.Errorf("first command = %v", request["execute"])
		}
		if err := encoder.Encode(map[string]any{"event": "RESET", "data": map[string]any{"guest": true}}); err != nil {
			return err
		}
		if err := encoder.Encode(map[string]any{"return": map[string]any{}, "id": request["id"]}); err != nil {
			return err
		}
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		if request["execute"] != "query-status" {
			return fmt.Errorf("second command = %v", request["execute"])
		}
		if err := encoder.Encode(map[string]any{"event": "RESUME", "data": map[string]any{}}); err != nil {
			return err
		}
		return encoder.Encode(map[string]any{"return": map[string]any{"running": true, "singlestep": false, "status": "running"}, "id": request["id"]})
	})

	var mu sync.Mutex
	events := make([]string, 0, 2)
	client := Client{Timeout: 2 * time.Second, OnEvent: func(event Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event.Name)
	}}
	status, err := client.QueryStatus(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.Status != "running" {
		t.Fatalf("status = %#v", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != "RESET" || events[1] != "RESUME" {
		t.Fatalf("events = %v", events)
	}
}

func TestQMPErrorIsPreserved(t *testing.T) {
	socket := startServer(t, func(decoder *json.Decoder, encoder *json.Encoder) error {
		if err := encoder.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{}, "capabilities": []any{}}}); err != nil {
			return err
		}
		var request map[string]any
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		if err := encoder.Encode(map[string]any{"return": map[string]any{}, "id": request["id"]}); err != nil {
			return err
		}
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		return encoder.Encode(map[string]any{"error": map[string]any{"class": "CommandNotFound", "desc": "missing"}, "id": request["id"]})
	})
	client := Client{Timeout: 2 * time.Second}
	if _, err := client.QueryName(context.Background(), socket); err == nil {
		t.Fatal("QMP error unexpectedly ignored")
	}
}
