package server

import (
	"reflect"
	"testing"
)

// A Wisp container must stay alive for the whole contract so exec/shell can
// attach. createOptions must therefore always set a keep-alive command,
// regardless of the launch spec — otherwise a bare base image's default command
// exits, the container stops, and provisioning/exec fails with "container is
// not running". This guards against silently dropping the keep-alive.
func TestCreateOptionsAlwaysSetsKeepAliveCmd(t *testing.T) {
	// A zero-value spec (no image/limits/network) must still get the keep-alive.
	opts := createOptions(launchSpec{}, "contract-abc")

	if len(opts.Cmd) == 0 {
		t.Fatal("createOptions returned an empty Cmd; the container would exit immediately")
	}
	if !reflect.DeepEqual(opts.Cmd, keepAliveCmd) {
		t.Fatalf("createOptions Cmd = %v, want keep-alive %v", opts.Cmd, keepAliveCmd)
	}
}
