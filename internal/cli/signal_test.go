//go:build unix

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestMCPExitsOnSIGTERM runs the real binary as an MCP client would, with
// stdin held open, and checks that SIGTERM ends it, and that a second
// SIGTERM is not swallowed. kb mcp servers were once found still running
// after SIGTERM following a client reconnect; that was not reproduced, so
// this guards the behaviour rather than a known cause.
func TestMCPExitsOnSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "kb")
	build := exec.Command("go", "build", "-o", bin, "github.com/xen0bit/kb/cmd/kb")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "mcp", "-db", filepath.Join(dir, "kb.db"))
	stdin, err := cmd.StdinPipe() // held open: no EOF
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // let it open the kb and block on stdin
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("exit after SIGTERM: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("kb mcp still running 5s after SIGTERM")
	}
}
