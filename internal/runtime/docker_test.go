package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
)

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
