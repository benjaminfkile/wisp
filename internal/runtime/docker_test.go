package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
)

// TestIdleCommand verifies the keep-alive command is chosen for the container
// OS: Linux keeps the historical `tail -f /dev/null` byte-for-byte (that binary
// only exists on Linux), and Windows gets a Windows-runnable idle command.
func TestIdleCommand(t *testing.T) {
	tests := []struct {
		name string
		os   ContainerOS
		want []string
	}{
		{"linux", OSLinux, []string{"tail", "-f", "/dev/null"}},
		{"windows", OSWindows, []string{"cmd", "/c", "ping -t localhost >NUL"}},
		// An unset/unknown OS falls back to the Linux command, matching the
		// daemon-default that ContainerOS detection also falls back to.
		{"unset falls back to linux", ContainerOS(""), []string{"tail", "-f", "/dev/null"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IdleCommand(tt.os); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("IdleCommand(%q) = %v, want %v", tt.os, got, tt.want)
			}
		})
	}
}

// TestShellCommand verifies a command string is wrapped in the OS shell: Linux
// keeps the historical `/bin/sh -c <cmd>` byte-for-byte (there is no /bin/sh on
// Windows), and a Windows daemon runs the command through `cmd /c <cmd>`.
func TestShellCommand(t *testing.T) {
	const cmd = "cd /repo && git diff"
	tests := []struct {
		name string
		os   ContainerOS
		want []string
	}{
		{"linux", OSLinux, []string{"/bin/sh", "-c", cmd}},
		{"windows", OSWindows, []string{"cmd", "/c", cmd}},
		// An unset/unknown OS falls back to the Linux wrapper, matching the
		// daemon-default that ContainerOS detection also falls back to.
		{"unset falls back to linux", ContainerOS(""), []string{"/bin/sh", "-c", cmd}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShellCommand(tt.os, cmd); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ShellCommand(%q, %q) = %v, want %v", tt.os, cmd, got, tt.want)
			}
		})
	}
}

// TestDefaultShell verifies the interactive PTY shell binary is chosen for the
// container OS: Linux keeps `/bin/sh` byte-for-byte, and a Windows daemon (which
// has no /bin/sh) launches `cmd.exe`.
func TestDefaultShell(t *testing.T) {
	tests := []struct {
		name string
		os   ContainerOS
		want []string
	}{
		{"linux", OSLinux, []string{"/bin/sh"}},
		{"windows", OSWindows, []string{"cmd.exe"}},
		{"unset falls back to linux", ContainerOS(""), []string{"/bin/sh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultShell(tt.os); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("DefaultShell(%q) = %v, want %v", tt.os, got, tt.want)
			}
		})
	}
}

// infoStubClient embeds the full Docker APIClient interface (left nil) and
// overrides only Info, so a test can drive DockerRuntime's OS detection without
// a real daemon. Any other method call would panic, which is fine: the OS
// detection path only touches Info.
type infoStubClient struct {
	client.APIClient
	info    system.Info
	infoErr error
}

func (c infoStubClient) Info(context.Context) (system.Info, error) {
	return c.info, c.infoErr
}

// TestDockerRuntimeContainerOS verifies the DockerRuntime reports the container
// OS taken from the daemon's stubbed Info.OSType.
func TestDockerRuntimeContainerOS(t *testing.T) {
	tests := []struct {
		name   string
		osType string
		want   ContainerOS
	}{
		{"linux", "linux", OSLinux},
		{"windows", "windows", OSWindows},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDockerRuntimeWithClient(infoStubClient{info: system.Info{OSType: tt.osType}})
			if got := d.ContainerOS(); got != tt.want {
				t.Fatalf("ContainerOS() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIsImageOSMismatch verifies the daemon's OS/platform rejection is
// recognized (case-insensitively) while unrelated errors and nil are not, so a
// higher layer can map only genuine mismatches to a clear OS-aware error.
func TestIsImageOSMismatch(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"windows on linux", errors.New(`image operating system "windows" cannot be used on this platform`), true},
		{"linux on windows", errors.New(`image operating system "linux" cannot be used on this platform`), true},
		{"uppercased", errors.New(`Image Operating System "windows" Cannot Be Used On This Platform`), true},
		{"operating-system/platform shape", errors.New("the container operating system does not match the host platform"), true},
		// The modern daemon rejects a Linux-only image on a windows-mode host at
		// pull time with this "no matching manifest ... manifest list entries"
		// shape (the exact message observed on a 4.61-era Docker Desktop).
		{"no matching manifest windows", errors.New(`no matching manifest for windows(10.0.26200)/amd64 in the manifest list entries`), true},
		// The linux-mode equivalent (a windows/arm image on a linux host) matches too.
		{"no matching manifest linux", errors.New(`no matching manifest for linux/amd64 in the manifest list entries`), true},
		// The wrapped chain the pull path actually produces still matches, since
		// the matcher is substring-based on err.Error().
		{"no matching manifest wrapped", fmt.Errorf("runtime: pull image ghcr.io/x/y: %w", errors.New(`Error response from daemon: no matching manifest for windows(10.0.26200)/amd64 in the manifest list entries`)), true},
		{"unrelated", errors.New("runtime: create container: no such image"), false},
		// "no matching manifest" WITHOUT the "manifest list entries" tail is not
		// enough to classify as a mismatch, so this stays false.
		{"partial no matching manifest", errors.New("no matching manifest for linux/amd64"), false},
		{"pull access denied", errors.New("pull access denied for foo/bar, repository does not exist or may require authentication"), false},
		{"no such image", errors.New("Error: No such image: foo:bar"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsImageOSMismatch(tt.err); got != tt.want {
				t.Errorf("IsImageOSMismatch(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestDockerRuntimeContainerOSFallback verifies detection is best-effort: an
// Info error or an unrecognized OSType falls back to linux, the Docker default.
func TestDockerRuntimeContainerOSFallback(t *testing.T) {
	t.Run("info error", func(t *testing.T) {
		d := NewDockerRuntimeWithClient(infoStubClient{infoErr: errors.New("no daemon")})
		if got := d.ContainerOS(); got != OSLinux {
			t.Fatalf("ContainerOS() with Info error = %q, want %q", got, OSLinux)
		}
	})
	t.Run("unknown ostype", func(t *testing.T) {
		d := NewDockerRuntimeWithClient(infoStubClient{info: system.Info{OSType: "plan9"}})
		if got := d.ContainerOS(); got != OSLinux {
			t.Fatalf("ContainerOS() with unknown OSType = %q, want %q", got, OSLinux)
		}
	})
}
