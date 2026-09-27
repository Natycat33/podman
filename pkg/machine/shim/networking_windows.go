package shim

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"go.podman.io/podman/v6/pkg/machine"
	"go.podman.io/podman/v6/pkg/machine/define"
	"go.podman.io/podman/v6/pkg/machine/env"
	"go.podman.io/podman/v6/pkg/machine/provider"
	sc "go.podman.io/podman/v6/pkg/machine/sockets"
	"go.podman.io/podman/v6/pkg/machine/vmconfigs"
	"golang.org/x/sys/windows"
)

func setGvproxyProcessAttributes(c *exec.Cmd) {
	// Set SysProcAttr DETACHED_PROCESS or the gvproxy process may be killed
	// when the parent window is closed.
	// This should not happen because gvproxy is built as a Windows GUI application
	// and doesn't inherit the parent console. But a console version of gvproxy is
	// also available, and using DETACHED_PROCESS makes sure that the behavior
	// is the same nevertheless.
	c.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS,
	}
}

func cleanupStaleHostForwarder(mc *vmconfigs.MachineConfig, provider vmconfigs.VMProvider) error {
	if provider.VMType() == define.WSLVirt {
		if err := machine.CleanupStaleWinProxy(mc.Name, provider.VMType()); err != nil {
			return fmt.Errorf("could not recover api proxy for %s: %w", env.WithPodmanPrefix(mc.Name), err)
		}
		return nil
	}
	if provider.UseProviderNetworkSetup() {
		return nil
	}

	dirs, err := env.GetMachineDirs(provider.VMType())
	if err != nil {
		return err
	}
	pidFile, err := dirs.RuntimeDir.AppendToNewVMFile("gvproxy.pid", nil)
	if err != nil {
		return err
	}

	pipeName := env.WithPodmanPrefix(mc.Name)
	if err := machine.CleanupStaleGVProxy(pipeName, *pidFile); err != nil {
		return fmt.Errorf("could not recover api proxy for %s: %w", pipeName, err)
	}
	return nil
}

// The gvproxy process will always terminate when the user logs off, but a hyperv vm will remain running.
// This will recover the gvproxy process in that situation
func EnsureHostForwarder() error {
	provider, err := provider.GetByVMType(define.HyperVVirt)
	if err != nil {
		return err
	}

	dirs, err := env.GetMachineDirs(provider.VMType())
	if err != nil {
		return err
	}

	mcs, err := vmconfigs.LoadMachinesInDir(dirs)
	if err != nil {
		return err
	}

	for _, mc := range mcs {
		state, err := provider.State(mc, true)
		if err != nil {
			return err
		}
		if state != define.Running {
			continue
		}

		pipeName := env.WithPodmanPrefix(mc.Name)
		if hostForwarderIsHealthy(pipeName) {
			return nil
		}

		pidFile, err := dirs.RuntimeDir.AppendToNewVMFile("gvproxy.pid", nil)
		if err != nil {
			return err
		}
		if err := machine.CleanupGVProxy(*pidFile); err != nil {
			return fmt.Errorf("stopping unhealthy api proxy for machine %q: %w", mc.Name, err)
		}

		hostSocks, _, _, err := setupMachineSockets(mc, dirs)
		if err == nil {
			err = startHostForwarder(mc, provider, dirs, hostSocks)
		}
		if err == nil {
			err = machine.WaitPipeExists(pipeName, 20, func() error { return nil })
		}
		if err == nil && !hostForwarderIsHealthy(pipeName) {
			err = fmt.Errorf("api proxy did not accept connections on named pipe %q", pipeName)
		}
		if err != nil {
			_ = machine.CleanupGVProxy(*pidFile)
			return fmt.Errorf("starting api proxy for machine %q: %w", mc.Name, err)
		}
		return nil
	}
	return nil
}

func hostForwarderIsHealthy(pipeName string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := machine.DialNamedPipe(ctx, `\\.\pipe\`+pipeName)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func setupMachineSockets(mc *vmconfigs.MachineConfig, _ *define.MachineDirs) ([]string, string, machine.APIForwardingState, error) {
	machinePipe := env.WithPodmanPrefix(mc.Name)
	if !machine.PipeNameAvailable(machinePipe, machine.MachineNameWait) {
		return nil, "", 0, fmt.Errorf("could not start api proxy since expected pipe is not available: %s", machinePipe)
	}
	sockets := []string{machine.NamedPipePrefix + machinePipe}
	state := machine.MachineLocal

	if machine.PipeNameAvailable(machine.GlobalNamedPipe, machine.GlobalNameWait) {
		sockets = append(sockets, machine.NamedPipePrefix+machine.GlobalNamedPipe)
		state = machine.DockerGlobal
	}

	hostSocket, err := mc.APISocket()
	if err != nil {
		return nil, "", 0, err
	}

	hostURL, err := sc.ToUnixURL(hostSocket)
	if err != nil {
		return nil, "", 0, err
	}
	sockets = append(sockets, hostURL.String())

	return sockets, sockets[len(sockets)-2], state, nil
}
