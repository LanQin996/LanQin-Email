//go:build linux

package maintenancehost

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var peerChildMode = flag.String("host-peer-child-mode", "", "synthetic maintenance host peer test helper")
var peerChildSocket = flag.String("host-peer-child-socket", "", "synthetic peer helper socket")

func TestHostPeerChild(t *testing.T) {
	if *peerChildMode == "" {
		t.Skip("private subprocess helper")
	}
	if os.Geteuid() != 1001 {
		t.Fatalf("synthetic child did not drop UID: %d", os.Geteuid())
	}
	switch *peerChildMode {
	case "denied-client":
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", *peerChildSocket)
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		res, err := client.Get("http://synthetic/v1/plans")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatalf("server allowed foreign kernel UID: %d", res.StatusCode)
		}
	case "nonroot-server":
		listener, err := net.Listen("unix", *peerChildSocket)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"items":[]}`)
		})}
		_ = server.Serve(listener)
	default:
		t.Fatal("unknown helper mode")
	}
}

func requireLinuxRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires actual Linux root for synthetic ownership and SO_PEERCRED")
	}
}

func waitHost(t *testing.T, client *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := client.Plans(context.Background()); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("root host socket did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHostLinuxRealUnixSocketRootPeerAndIdempotence(t *testing.T) {
	requireLinuxRoot(t)
	cfg := hostFixture(t)
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, cfg, func(_ context.Context, _ Request, _ ApprovedPlan, phase func(string) error) (Outcome, error) {
			calls.Add(1)
			if err := phase("maintenance"); err != nil {
				return Outcome{}, err
			}
			return Outcome{State: "succeeded"}, nil
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	client := NewClient(cfg.Socket)
	waitHost(t, client)
	plans, err := client.Plans(ctx)
	if err != nil || len(plans) != 1 || plans[0].ID != cfg.Plans[0].ID || plans[0].Driver != "sqlite" {
		t.Fatalf("actual root peer plans: %+v %v", plans, err)
	}
	req := hostRequest(cfg, 1)
	if _, err := client.Submit(ctx, req); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := client.Status(ctx, req.ID)
		if err == nil && status.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("actual Unix peer operation did not finish: %+v %v", status, err)
		}
		time.Sleep(time.Millisecond)
	}
	if status, err := client.Submit(ctx, req); err != nil || status.State != "succeeded" || calls.Load() != 1 {
		t.Fatalf("duplicate was executed again: %+v err=%v calls=%d", status, err, calls.Load())
	}
	if _, err := client.Status(ctx, strings.Repeat("e", 32)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request status: %v", err)
	}
}

func accessiblePeerHelper(t *testing.T, cfg Config) string {
	t.Helper()
	root := filepath.Dir(filepath.Dir(cfg.Socket))
	// Only task-owned fixture ancestors are made traversable, never writable.
	for p := root; p != os.TempDir() && p != filepath.Dir(p); p = filepath.Dir(p) {
		if !strings.HasPrefix(p, os.TempDir()+string(filepath.Separator)) {
			t.Fatal("fixture escaped task temp directory")
		}
		if err := os.Chmod(p, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Dir(cfg.Socket), 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "synthetic-peer-helper.test")
	if err := os.WriteFile(helper, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return helper
}

func peerCommand(binary, mode, socket string) *exec.Cmd {
	cmd := exec.Command(binary, "-test.run=^TestHostPeerChild$", "-host-peer-child-mode="+mode, "-host-peer-child-socket="+socket)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1001, Gid: 1001, NoSetGroups: true}}
	return cmd
}

func TestHostLinuxKernelPeerUIDRejectsForeignClient(t *testing.T) {
	requireLinuxRoot(t)
	cfg := hostFixture(t)
	helper := accessiblePeerHelper(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, cfg, func(context.Context, Request, ApprovedPlan, func(string) error) (Outcome, error) {
			return Outcome{State: "succeeded"}, nil
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	waitHost(t, NewClient(cfg.Socket))
	if output, err := peerCommand(helper, "denied-client", cfg.Socket).CombinedOutput(); err != nil {
		t.Fatalf("foreign-UID real kernel peer test: %v\n%s", err, output)
	}
}

func TestHostLinuxClientRejectsNonrootHelperPeer(t *testing.T) {
	requireLinuxRoot(t)
	cfg := hostFixture(t)
	helper := accessiblePeerHelper(t, cfg)
	nonroot := filepath.Join(filepath.Dir(filepath.Dir(cfg.Socket)), "nonroot-socket")
	if os.Mkdir(nonroot, 0o700) != nil || os.Chown(nonroot, 1001, 1001) != nil {
		t.Fatal("cannot set synthetic nonroot socket ownership")
	}
	socket := filepath.Join(nonroot, "fake.sock")
	cmd := peerCommand(helper, "nonroot-server", socket)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic nonroot helper socket did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := NewClient(socket).Plans(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("client accepted nonroot helper kernel peer: %v", err)
	}
}

func TestHostLinuxShutdownRetainsLockUntilExecutorCleanup(t *testing.T) {
	requireLinuxRoot(t)
	cfg := hostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, cfg, func(ctx context.Context, _ Request, _ ApprovedPlan, _ func(string) error) (Outcome, error) {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-release
			return Outcome{State: "needsRecovery", ErrorCode: "interrupted"}, ctx.Err()
		})
	}()
	defer cancel()
	client := NewClient(cfg.Socket)
	waitHost(t, client)
	if _, err := client.Submit(ctx, hostRequest(cfg, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("executor not entered")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("executor not cancelled")
	}
	select {
	case err := <-done:
		close(release)
		t.Fatalf("Serve returned before executor cleanup: %v", err)
	default:
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), time.Second)
	err := Serve(secondCtx, cfg, func(context.Context, Request, ApprovedPlan, func(string) error) (Outcome, error) {
		return Outcome{}, nil
	})
	secondCancel()
	if !errors.Is(err, ErrConflict) {
		close(release)
		t.Fatalf("daemon lock released before cleanup: %v", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve failed to exit after cleanup")
	}
}
