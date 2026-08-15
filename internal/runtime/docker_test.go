package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

// TestDockerRuntimeDaemonInfo verifies DaemonInfo is backed by the Docker
// client's Info call: it surfaces the daemon's registered runtime names (sorted)
// and container OS, mapping an unknown OSType to linux and wrapping an Info error.
func TestDockerRuntimeDaemonInfo(t *testing.T) {
	t.Run("runtimes and os", func(t *testing.T) {
		stub := infoStubClient{info: system.Info{
			OSType: "linux",
			Runtimes: map[string]system.RuntimeWithStatus{
				"runsc": {}, "runc": {}, "kata-runtime": {},
			},
		}}
		d := NewDockerRuntimeWithClient(stub)
		got, err := d.DaemonInfo(context.Background())
		if err != nil {
			t.Fatalf("DaemonInfo: %v", err)
		}
		want := []string{"kata-runtime", "runc", "runsc"} // sorted for stable output
		if !reflect.DeepEqual(got.Runtimes, want) {
			t.Errorf("Runtimes = %v, want %v", got.Runtimes, want)
		}
		if got.OS != OSLinux {
			t.Errorf("OS = %q, want linux", got.OS)
		}
	})
	t.Run("windows os", func(t *testing.T) {
		rt := NewDockerRuntimeWithClient(infoStubClient{info: system.Info{OSType: "windows"}})
		got, err := rt.DaemonInfo(context.Background())
		if err != nil {
			t.Fatalf("DaemonInfo: %v", err)
		}
		if got.OS != OSWindows {
			t.Errorf("OS = %q, want windows", got.OS)
		}
	})
	t.Run("info error is wrapped", func(t *testing.T) {
		rt := NewDockerRuntimeWithClient(infoStubClient{infoErr: errors.New("no daemon")})
		if _, err := rt.DaemonInfo(context.Background()); err == nil {
			t.Fatal("DaemonInfo with Info error = nil, want an error")
		}
	})
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

// createStubClient embeds the Docker APIClient (left nil) and overrides only the
// two methods DockerRuntime.Create needs without a daemon: Info drives OS
// detection at construction, and ContainerCreate captures the HostConfig so a
// test can assert how the isolation level maps onto it. Any other method call
// would panic, which is fine: Create only touches these.
type createStubClient struct {
	client.APIClient
	osType    string
	gotHost   *container.HostConfig
	gotConfig *container.Config
}

func (c *createStubClient) Info(context.Context) (system.Info, error) {
	return system.Info{OSType: c.osType}, nil
}

func (c *createStubClient) ContainerCreate(_ context.Context, config *container.Config, hostConfig *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	c.gotConfig = config
	c.gotHost = hostConfig
	return container.CreateResponse{ID: "created"}, nil
}

// TestDockerRuntimeCreateIsolation verifies DockerRuntime.Create maps the
// requested isolation level onto the container's HostConfig the same way for the
// real backend: shared (and the empty default) leave Runtime and Isolation unset
// (default runc); sandboxed selects the gVisor runtime "runsc"; vm on a linux
// daemon selects the Kata runtime "kata-runtime"; vm on a windows daemon selects
// Hyper-V isolation "hyperv". Runtime and Isolation are never both set, and no
// other HostConfig field (SecurityOpt, Resources, NetworkMode) is disturbed.
func TestDockerRuntimeCreateIsolation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name          string
		osType        string
		isolation     string
		wantRuntime   string
		wantIsolation container.Isolation
	}{
		{"shared linux", "linux", IsolationShared, "", ""},
		{"empty defaults to shared", "linux", "", "", ""},
		{"shared windows", "windows", IsolationShared, "", ""},
		{"sandboxed linux", "linux", IsolationSandboxed, "runsc", ""},
		{"sandboxed windows", "windows", IsolationSandboxed, "runsc", ""},
		{"vm linux", "linux", IsolationVM, "kata-runtime", ""},
		{"vm windows", "windows", IsolationVM, "", container.Isolation("hyperv")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &createStubClient{osType: tt.osType}
			d := NewDockerRuntimeWithClient(stub)

			// Pass unrelated HostConfig-bearing options to confirm they survive
			// untouched alongside the isolation mapping.
			opts := CreateOptions{
				Isolation:   tt.isolation,
				SecurityOpt: []string{"no-new-privileges:true"},
				NetworkMode: "none",
				Resources:   Resources{NanoCPUs: 1_500_000_000, MemoryBytes: 256 << 20},
			}
			if _, err := d.Create(ctx, "img", opts); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if stub.gotHost == nil {
				t.Fatal("ContainerCreate was not called")
			}
			if stub.gotHost.Runtime != tt.wantRuntime {
				t.Errorf("HostConfig.Runtime = %q, want %q", stub.gotHost.Runtime, tt.wantRuntime)
			}
			if stub.gotHost.Isolation != tt.wantIsolation {
				t.Errorf("HostConfig.Isolation = %q, want %q", stub.gotHost.Isolation, tt.wantIsolation)
			}
			if stub.gotHost.Runtime != "" && stub.gotHost.Isolation != "" {
				t.Errorf("both Runtime (%q) and Isolation (%q) set; must never both be set", stub.gotHost.Runtime, stub.gotHost.Isolation)
			}
			// no-new-privileges (SecurityOpt) is a Linux-only option that a Windows
			// daemon rejects, so it is applied on Linux and dropped on Windows.
			var wantSecOpt []string
			if tt.osType != "windows" {
				wantSecOpt = []string{"no-new-privileges:true"}
			}
			if !reflect.DeepEqual(stub.gotHost.SecurityOpt, wantSecOpt) {
				t.Errorf("SecurityOpt = %v, want %v", stub.gotHost.SecurityOpt, wantSecOpt)
			}
			if stub.gotHost.NetworkMode != "none" {
				t.Errorf("NetworkMode = %q, want unchanged \"none\"", stub.gotHost.NetworkMode)
			}
			if stub.gotHost.Resources.NanoCPUs != 1_500_000_000 || stub.gotHost.Resources.Memory != 256<<20 {
				t.Errorf("Resources = %+v, want unchanged", stub.gotHost.Resources)
			}
		})
	}
}

// TestDockerRuntimeCreateGPUDeviceRequests verifies DockerRuntime.Create maps the
// exclusively-assigned GPU device IDs onto HostConfig.Resources.DeviceRequests —
// the nvidia driver, the explicit device IDs, and the "gpu" capability (the SDK
// equivalent of `docker run --gpus device=...`) — for the shared/runc launch
// mechanism, and leaves DeviceRequests nil for a GPU-less lease.
func TestDockerRuntimeCreateGPUDeviceRequests(t *testing.T) {
	ctx := context.Background()

	t.Run("shared attaches assigned devices", func(t *testing.T) {
		stub := &createStubClient{osType: "linux"}
		d := NewDockerRuntimeWithClient(stub)
		opts := CreateOptions{
			Isolation: IsolationShared,
			Resources: Resources{GPUDeviceIDs: []string{"GPU-a", "GPU-b"}},
		}
		if _, err := d.Create(ctx, "img", opts); err != nil {
			t.Fatalf("Create: %v", err)
		}
		want := []container.DeviceRequest{{
			Driver:       "nvidia",
			DeviceIDs:    []string{"GPU-a", "GPU-b"},
			Capabilities: [][]string{{"gpu"}},
		}}
		if !reflect.DeepEqual(stub.gotHost.Resources.DeviceRequests, want) {
			t.Fatalf("DeviceRequests = %+v, want %+v", stub.gotHost.Resources.DeviceRequests, want)
		}
	})

	t.Run("no gpus leaves DeviceRequests nil", func(t *testing.T) {
		stub := &createStubClient{osType: "linux"}
		d := NewDockerRuntimeWithClient(stub)
		if _, err := d.Create(ctx, "img", CreateOptions{Isolation: IsolationShared}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if stub.gotHost.Resources.DeviceRequests != nil {
			t.Fatalf("DeviceRequests = %+v, want nil for a GPU-less lease", stub.gotHost.Resources.DeviceRequests)
		}
	})

	t.Run("gpus at a vm launch mechanism fail the create", func(t *testing.T) {
		stub := &createStubClient{osType: "linux"}
		d := NewDockerRuntimeWithClient(stub)
		opts := CreateOptions{
			Isolation: IsolationVM,
			Resources: Resources{GPUDeviceIDs: []string{"GPU-a"}},
		}
		if _, err := d.Create(ctx, "img", opts); !errors.Is(err, ErrGPUAttachUnsupported) {
			t.Fatalf("Create error = %v, want ErrGPUAttachUnsupported", err)
		}
	})
}

// killStubClient embeds the Docker APIClient (left nil) and overrides only
// Info (for construction) and ContainerRemove (what Kill calls). A test sets
// removeErr to the error the daemon should return.
type killStubClient struct {
	client.APIClient
	removeErr error
}

func (c *killStubClient) Info(context.Context) (system.Info, error) {
	return system.Info{OSType: "linux"}, nil
}

func (c *killStubClient) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	return c.removeErr
}

// TestDockerRuntimeKillMapsNotFound verifies DockerRuntime.Kill maps the docker
// client's not-found error to runtime.ErrNotFound, the same way Inspect does —
// so reaper.expire's errors.Is(err, ErrNotFound) suppression correctly covers
// containers removed out of band (the LivenessGone case the container-death
// detection was built for) rather than logging a spurious ERROR every time.
// Other errors are wrapped through unchanged.
func TestDockerRuntimeKillMapsNotFound(t *testing.T) {
	ctx := context.Background()

	t.Run("docker not-found maps to ErrNotFound", func(t *testing.T) {
		stub := &killStubClient{removeErr: errdefs.NotFound(errors.New("No such container: abc"))}
		d := NewDockerRuntimeWithClient(stub)
		if err := d.Kill(ctx, "abc"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Kill error = %v, want to match ErrNotFound", err)
		}
	})

	t.Run("other errors are wrapped", func(t *testing.T) {
		boom := errors.New("daemon unreachable")
		stub := &killStubClient{removeErr: boom}
		d := NewDockerRuntimeWithClient(stub)
		err := d.Kill(ctx, "abc")
		if err == nil {
			t.Fatal("Kill returned nil, want an error")
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatalf("Kill error = %v, must not match ErrNotFound for a non-not-found failure", err)
		}
		if !errors.Is(err, boom) {
			t.Fatalf("Kill error = %v, want to wrap %v", err, boom)
		}
	})

	t.Run("no error passes through", func(t *testing.T) {
		stub := &killStubClient{}
		d := NewDockerRuntimeWithClient(stub)
		if err := d.Kill(ctx, "abc"); err != nil {
			t.Fatalf("Kill = %v, want nil on a successful remove", err)
		}
	})
}
