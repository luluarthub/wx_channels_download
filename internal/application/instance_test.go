package application

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDatabaseInstanceRejectsSecondHandleAndAllowsRelease(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "instance.db")
	first, err := acquireDatabaseInstance(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Keep the dot-dot form until the production path normalization executes.
	alias := dir + string(os.PathSeparator) + "unused" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "instance.db"
	if second, err := acquireDatabaseInstance(alias); !errors.Is(err, errDatabaseInstanceRunning) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second handle: got %v, want instance-running error", err)
	}
	if runtime.GOOS == "windows" {
		if second, err := acquireDatabaseInstance(strings.ToUpper(dbPath)); !errors.Is(err, errDatabaseInstanceRunning) {
			if second != nil {
				_ = second.Close()
			}
			t.Fatalf("case alias: got %v, want instance-running error", err)
		}
	}
	other, err := acquireDatabaseInstance(filepath.Join(dir, "other.db"))
	if err != nil {
		t.Fatalf("independent database: %v", err)
	}
	_ = other.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("repeat close: %v", err)
	}
	next, err := acquireDatabaseInstance(dbPath)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	_ = next.Close()
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acquiring lock must not create a database: %v", err)
	}
}

func instanceHelperCommand(t *testing.T, dbPath, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDatabaseInstanceHelperProcess$")
	cmd.Env = append(os.Environ(), "WX_CHANNEL_INSTANCE_TEST_PATH="+dbPath, "WX_CHANNEL_INSTANCE_TEST_MODE="+mode)
	return cmd
}

func TestDatabaseInstanceRejectsSecondProcessAndAllowsRelease(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "process.db")
	first, err := acquireDatabaseInstance(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if out, err := instanceHelperCommand(t, dbPath, "blocked").CombinedOutput(); err != nil {
		t.Fatalf("second process was not rejected: %v\n%s", err, out)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := instanceHelperCommand(t, dbPath, "acquire").CombinedOutput(); err != nil {
		t.Fatalf("process could not reacquire released lock: %v\n%s", err, out)
	}
}

func TestDatabaseInstanceReleasedAfterProcessDeath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crash.db")
	cmd := instanceHelperCommand(t, dbPath, "hold")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "locked" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("child did not acquire its lock: %q, %v", scanner.Text(), scanner.Err())
	}
	if second, err := acquireDatabaseInstance(dbPath); !errors.Is(err, errDatabaseInstanceRunning) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("child lock was not exclusive: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lock, err := acquireDatabaseInstance(dbPath)
	if err != nil {
		t.Fatalf("OS did not release lock after process death: %v", err)
	}
	_ = lock.Close()
}

func TestDatabaseInstanceHelperProcess(t *testing.T) {
	mode := os.Getenv("WX_CHANNEL_INSTANCE_TEST_MODE")
	if mode == "" {
		return
	}
	lock, err := acquireDatabaseInstance(os.Getenv("WX_CHANNEL_INSTANCE_TEST_PATH"))
	if mode == "blocked" {
		if !errors.Is(err, errDatabaseInstanceRunning) {
			if lock != nil {
				_ = lock.Close()
			}
			t.Fatalf("expected instance-running error, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if mode == "hold" {
		fmt.Println("locked")
		time.Sleep(time.Minute)
	}
}

func TestCanonicalDatabasePathResolvesParentSymlinks(t *testing.T) {
	dir := t.TempDir()
	realDir, alias := filepath.Join(dir, "real"), filepath.Join(dir, "alias")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlink creation not available: %v", err)
	}
	first, err := acquireDatabaseInstance(filepath.Join(realDir, "future.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := acquireDatabaseInstance(filepath.Join(alias, "future.db")); !errors.Is(err, errDatabaseInstanceRunning) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("symlink alias did not share lock: %v", err)
	}
}
