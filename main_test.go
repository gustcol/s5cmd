//go:build !windows

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestTerminatedBySignal checks that when s5cmd is interrupted it ends up
// killed by that same signal, rather than exiting with a status code. See
// https://github.com/peak/s5cmd/issues/615.
func TestTerminatedBySignal(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "s5cmd")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building s5cmd: %v\n%s", err, out)
	}

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		sig := sig
		t.Run(sig.String(), func(t *testing.T) {
			// A server that accepts connections and never answers, so that
			// s5cmd stays blocked waiting for a response.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()

			accepted := make(chan net.Conn, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					close(accepted)
					return
				}
				accepted <- conn
			}()

			cmd := exec.Command(bin,
				"--endpoint-url", "http://"+ln.Addr().String(),
				"--retry-count", "0",
				"ls", "s3://bucket",
			)
			cmd.Env = append(os.Environ(),
				"AWS_ACCESS_KEY_ID=test",
				"AWS_SECRET_ACCESS_KEY=test",
				"AWS_REGION=us-east-1",
			)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Make sure the child never outlives the test.
			defer func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}()

			select {
			case conn, ok := <-accepted:
				if !ok {
					t.Fatal("listener closed before s5cmd connected")
				}
				defer conn.Close()
			case <-time.After(30 * time.Second):
				t.Fatal("s5cmd did not connect to the test server")
			}

			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}

			waitErr := make(chan error, 1)
			go func() { waitErr <- cmd.Wait() }()

			select {
			case <-waitErr:
			case <-time.After(30 * time.Second):
				t.Fatalf("s5cmd did not exit after %v", sig)
			}

			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok {
				t.Fatalf("unexpected process state type %T", cmd.ProcessState.Sys())
			}
			if !status.Signaled() || status.Signal() != sig {
				t.Errorf("want process killed by %v, got %v", sig, cmd.ProcessState)
			}
		})
	}
}
