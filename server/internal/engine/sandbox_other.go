//go:build !unix

package engine

import "os/exec"

// The playground ships as a Linux container; these stubs exist so the package
// still builds on other platforms for editor tooling and `go vet`. Process
// group isolation is unavailable here, so a run falls back to killing the
// single process — acceptable for local development, not for serving traffic.

func configureSandbox(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
