package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeRunner is a CommandRunner that returns canned output/error and records the
// command it was asked to run, so enumeration is tested without a real nvidia-smi
// (the CI/runner container has neither a GPU nor the tooling).
type fakeRunner struct {
	out     []byte
	err     error
	gotName string
	gotArgs []string
}

func (f *fakeRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.gotName = name
	f.gotArgs = args
	return f.out, f.err
}

// TestEnumerateGPUsParsesDevices verifies a successful nvidia-smi enumeration is
// parsed into devices with the UUID preserved, the class normalized, and the
// memory read as an integer count of mebibytes.
func TestEnumerateGPUsParsesDevices(t *testing.T) {
	runner := &fakeRunner{out: []byte(
		"GPU-1b2c3d4e-0000, NVIDIA GeForce RTX 4090, 24564\n" +
			"GPU-aaaa-bbbb, Tesla V100-SXM2-16GB, 16160\n")}

	got, err := enumerateGPUs(context.Background(), runner)
	if err != nil {
		t.Fatalf("enumerateGPUs: unexpected error: %v", err)
	}
	want := []GPUDevice{
		{ID: "GPU-1b2c3d4e-0000", Class: "nvidia-geforce-rtx-4090", VRAMMB: 24564},
		{ID: "GPU-aaaa-bbbb", Class: "tesla-v100-sxm2-16gb", VRAMMB: 16160},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("enumerateGPUs = %+v, want %+v", got, want)
	}

	// The machine-readable query flags are exactly what the fake must stand in for.
	if runner.gotName != nvidiaSMIBinary {
		t.Errorf("ran %q, want %q", runner.gotName, nvidiaSMIBinary)
	}
	if !reflect.DeepEqual(runner.gotArgs, []string{nvidiaSMIQueryGPU, nvidiaSMIFormat}) {
		t.Errorf("args = %v, want the machine-readable query flags", runner.gotArgs)
	}
}

// TestEnumerateGPUsCommandFailure verifies that a runner error (nvidia-smi absent
// or exiting non-zero — the GPU-less host) surfaces as an error the caller
// degrades to "no GPU support", not a panic or empty success.
func TestEnumerateGPUsCommandFailure(t *testing.T) {
	sentinel := errors.New("exec: nvidia-smi: executable file not found in $PATH")
	runner := &fakeRunner{err: sentinel}

	got, err := enumerateGPUs(context.Background(), runner)
	if err == nil {
		t.Fatalf("enumerateGPUs = %+v, want an error when the command fails", got)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap the runner error", err)
	}
}

// TestEnumerateGPUsGarbage verifies malformed output is rejected as an error
// (degrading to no GPU support) rather than being silently skipped — a host only
// advertises GPUs it could fully describe.
func TestEnumerateGPUsGarbage(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{"too few fields", "GPU-1, NVIDIA GeForce RTX 4090\n"},
		{"non-integer memory", "GPU-1, NVIDIA GeForce RTX 4090, twenty-four-gigs\n"},
		{"empty uuid", ", NVIDIA GeForce RTX 4090, 24564\n"},
		{"empty name", "GPU-1, , 24564\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := enumerateGPUs(context.Background(), &fakeRunner{out: []byte(tc.out)}); err == nil {
				t.Errorf("enumerateGPUs(%q) = nil error, want an error for garbage", tc.out)
			}
		})
	}
}

// TestEnumerateGPUsEmpty verifies enumeration succeeding with no device lines
// yields an empty, non-nil slice and no error (the host has the tooling but no
// GPUs; policy still treats it as unsupported since no device was enumerated).
func TestEnumerateGPUsEmpty(t *testing.T) {
	got, err := enumerateGPUs(context.Background(), &fakeRunner{out: []byte("\n  \n")})
	if err != nil {
		t.Fatalf("enumerateGPUs: unexpected error: %v", err)
	}
	if got == nil {
		t.Errorf("enumerateGPUs = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("enumerateGPUs = %+v, want empty", got)
	}
}

// TestNormalizeGPUClass verifies the wire "class" normalization: lowercase with
// whitespace runs collapsed to single hyphens, per the wire contract.
func TestNormalizeGPUClass(t *testing.T) {
	cases := map[string]string{
		"NVIDIA GeForce RTX 4090": "nvidia-geforce-rtx-4090",
		"  Tesla  T4  ":           "tesla-t4",
		"NVIDIA A100-SXM4-80GB":   "nvidia-a100-sxm4-80gb",
		"NVIDIA H100 PCIe":        "nvidia-h100-pcie",
		"already-normalized":      "already-normalized",
		"NVIDIA\tRTX\n6000":       "nvidia-rtx-6000",
	}
	for in, want := range cases {
		if got := normalizeGPUClass(in); got != want {
			t.Errorf("normalizeGPUClass(%q) = %q, want %q", in, got, want)
		}
	}
}
