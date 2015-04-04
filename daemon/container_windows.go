package daemon

import (
	"fmt"
	"time"

	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/pkg/archive"
)

func (container *Container) Kill() error {
	if !container.IsRunning() {
		return nil
	}

	// 1. Send SIGKILL
	if err := container.killPossiblyDeadProcess(9); err != nil {
		return err
	}

	// 2. Wait for the process to die, in last resort, try to kill the process directly
	// TODO Windows: Equivalent to Linux

	container.WaitStop(-1 * time.Second)
	return nil
}

// TODO WINDOWS - This function still needs more refactoring.
func (container *Container) Start() (err error) {
	container.Lock()
	defer container.Unlock()

	if container.Running {
		return nil
	}

	// if we encounter an error during start we need to ensure that any other
	// setup has been cleaned up properly
	defer func() {
		if err != nil {
			container.setError(err)
			// if no one else has set it, make sure we don't leave it at zero
			if container.ExitCode == 0 {
				container.ExitCode = 128
			}
			container.toDisk()
			container.cleanup()
		}
	}()

	if err := container.Mount(); err != nil {
		return err
	}

	// This is where is Linux it calls into the Exec Driver. TODO WINDOWS (see populateCommand())
	if err := populateCommand(container, nil); err != nil {
		return err
	}

	return container.waitForStart()
}

// TODO WINDOWS
// This can be totally factored out but currently also used in create.go
func (container *Container) prepareVolumes() error {
	return nil
}

// TODO WINDOWS
// GetSize, return real size, virtual size
func (container *Container) GetSize() (int64, int64) {
	return 0, 0
}

func (container *Container) ExportRw() (archive.Archive, error) {
	if container.IsRunning() {
		return nil, fmt.Errorf("Cannot export a running container.")
	}
	return nil, nil
}

func populateCommand(c *Container, env []string) error {

	pid := &execdriver.Pid{}
	pid.HostPid = c.hostConfig.PidMode.IsHost()

	resources := &execdriver.Resources{
		Memory:     c.hostConfig.Memory,
		MemorySwap: c.hostConfig.MemorySwap,
		CpuShares:  c.hostConfig.CpuShares,
		CpusetCpus: c.hostConfig.CpusetCpus,
	}

	processConfig := execdriver.ProcessConfig{
		Privileged: c.hostConfig.Privileged,
		Entrypoint: c.Path,
		Arguments:  c.Args,
		Tty:        c.Config.Tty,
		User:       c.Config.User,
	}

	processConfig.Env = env

	c.command = &execdriver.Command{
		ID:             c.ID,
		Rootfs:         c.RootfsPath(),
		ReadonlyRootfs: c.hostConfig.ReadonlyRootfs,
		InitPath:       "/.dockerinit",
		WorkingDir:     c.Config.WorkingDir,
		//		Network:            en,
		//		Ipc:                ipc,
		Pid:       pid,
		Resources: resources,
		//		AllowedDevices:     allowedDevices,
		//		AutoCreatedDevices: autoCreatedDevices,
		CapAdd:        c.hostConfig.CapAdd,
		CapDrop:       c.hostConfig.CapDrop,
		ProcessConfig: processConfig,
		ProcessLabel:  c.GetProcessLabel(),
		MountLabel:    c.GetMountLabel(),
		//		LxcConfig:          lxcConfig,
		//		AppArmorProfile:    c.AppArmorProfile,
		Dummy: c.Config.Dummy,
	}

	return nil
}
