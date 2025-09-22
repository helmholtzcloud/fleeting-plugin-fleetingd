package fleetingd

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"time"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/sys/unix"
)

// Currently the number of VM slots is limited by the number of /30s, this is fairly arbitrary but should be plenty for now
const VMPrefix = "172.16.120."
const MaxIPAMSlots = 255 / 4

type InstanceGroup struct {
	EgressInterface              string   `json:"egress_interface"`
	VMDiskDir                    string   `json:"vm_disk_directory"`
	VMSubnet                     string   `json:"vm_subnet"`
	VMNumCPUCores                uint64   `json:"vm_num_cpu_cores"`
	VMMemoryMegabytes            uint64   `json:"vm_memory_mb"`
	VMDiskSizeGB                 uint64   `json:"vm_disk_size_gb"`
	VMPrebuildCloudinitExtraCmds []string `json:"vm_prebuild_cloudinit_extra_cmds"`
	VMEnableVirtioConsole        bool     `json:"vm_enable_virtio_console"`

	logger    hclog.Logger
	inventory *Inventory
}

func (i *InstanceGroup) Init(ctx context.Context, logger hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	//
	// Preflight checks
	//

	i.logger = logger.Named("fleetingd")

	i.inventory = NewInventory()

	// Check all supporting tools are installed
	requiredBinaries := []string{
		"cloud-hypervisor",
		"nft",
		"qemu-img",
	}

	for _, binary := range requiredBinaries {
		_, err := exec.LookPath(binary)
		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("could not find required binary %s on PATH, please check the PATH variable or install the missing tool: %w", binary, err)
		}
	}

	// Check disk dir is writable
	err := unix.Access(i.VMDiskDir, unix.W_OK)
	if err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("'%s' was specified as vm_disk_directory in the settings but is not writable: %w", i.VMDiskDir, err)
	}

	return provider.ProviderInfo{
		ID:        "fleetingd",
		MaxSize:   MaxIPAMSlots,
		Version:   Version.Version,
		BuildInfo: "TBD",
	}, nil
}

func (i *InstanceGroup) Update(ctx context.Context, updateFunc func(instance string, state provider.State)) error {
	// Query status from inventory
	instances := i.inventory.GetAllInstances()

	for _, instance := range instances {
		err := i.Heartbeat(ctx, instance)
		if err != nil {
			i.logger.Info("creating...", "instance", instance)
			updateFunc(instance, provider.StateCreating)
			continue
		}

		updateFunc(instance, provider.StateRunning)
	}

	return nil
}

func (i *InstanceGroup) Increase(ctx context.Context, n int) (succeeded int, err error) {
	// Try to boot more instances

	for counter := 0; counter < n; counter++ {
		err := i.inventory.BootInstance(i)
		if err != nil {
			i.logger.Error("instance boot error", "error", err)
			return counter, err
		}
	}

	return n, nil
}

func (i *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	// Try to remove instances
	removedInstances := []string{}

	for _, instanceToRemove := range instances {
		i.logger.Info("stopping instance", "instance", instanceToRemove)

		err := i.inventory.DestroyInstance(instanceToRemove)
		if err != nil {
			i.logger.Error("error stopping instance: %w", err)
			continue
		}

		i.logger.Info("stopped instance", "instance", instanceToRemove)

		removedInstances = append(removedInstances, instanceToRemove)
	}

	return removedInstances, nil
}

func (i *InstanceGroup) ConnectInfo(ctx context.Context, instance string) (provider.ConnectInfo, error) {
	// Return connection information from the inventory

	info, err := i.inventory.GetConnectInfo(instance)
	if err != nil {
		return provider.ConnectInfo{}, err
	}

	return *info, err
}

func (i *InstanceGroup) Heartbeat(ctx context.Context, instance string) error {
	// Check SSH connection
	info, err := i.inventory.GetConnectInfo(instance)
	if err != nil {
		return err
	}

	// Check SSH port is reachable
	hostPort := net.JoinHostPort(info.InternalAddr, strconv.Itoa(info.ProtocolPort))
	connection, err := net.DialTimeout("tcp", hostPort, time.Second)
	if err != nil {
		return err
	}
	connection.Close()

	return nil
}

func (i *InstanceGroup) Shutdown(ctx context.Context) error {
	// Destroy all instances
	return i.inventory.DestroyAllInstances()
}

func (i *InstanceGroup) MakeAddress(index int) string {
	return i.VMSubnet + strconv.Itoa(index)
}
