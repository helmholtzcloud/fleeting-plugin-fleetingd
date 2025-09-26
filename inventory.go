package fleetingd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"text/template"
	"time"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/crypto/ssh"
)

type InstanceInfo struct {
	Name                      string
	InstanceContextCancelFunc context.CancelFunc

	HostTapIP             string
	InstanceTapIP         string
	InstanceTapMacAddress string

	SSHPublicKey  ed25519.PublicKey
	SSHPrivateKey ed25519.PrivateKey
}

type Inventory struct {
	lock     *sync.RWMutex
	prebuild *sync.Once

	// Stop accepting requests when this is true
	shuttingDown bool

	// IPAM "tickets" / subnet tracking
	ipamSlots map[string]struct{}
	// Inventory
	instances map[string]*InstanceInfo
}

func NewInventory() *Inventory {
	return &Inventory{
		lock:     &sync.RWMutex{},
		prebuild: &sync.Once{},

		ipamSlots: make(map[string]struct{}),
		instances: make(map[string]*InstanceInfo),
	}
}

func (i *Inventory) RunPrebuild(instanceGroup *InstanceGroup) error {
	//
	// Disk image preparation
	//

	instanceGroup.logger.Info("First VM requested. Preparing environment...")

	// Clear old instance images
	err := instanceGroup.prepareWorkdir()
	if err != nil {
		return err
	}

	// Ensure disk images are present
	err = instanceGroup.ensureImages()
	if err != nil {
		return err
	}

	// Run prebuild
	instanceGroup.logger.Info("Triggering prebuild...")
	err = instanceGroup.inventory.PrebuildInstance(instanceGroup)
	if err != nil {
		return err
	}
	instanceGroup.logger.Info("Prebuild finished.")

	return nil
}

func (i *Inventory) BootInstance(instanceGroup *InstanceGroup) error {
	var err error

	i.prebuild.Do(func() {
		err = i.RunPrebuild(instanceGroup)
	})
	if err != nil {
		instanceGroup.logger.Error("Prebuild failed", err)
		return err
	}

	i.lock.RLock()
	takenSlots := len(i.ipamSlots)
	i.lock.RUnlock()

	// Short-circuit function instead of walking address space
	if takenSlots >= MaxIPAMSlots {
		return errors.New("available VM address space exhausted")
	}

	i.lock.Lock()

	if i.shuttingDown {
		i.lock.Unlock()
		return errors.New("system is shutting down")
	}

	// Behold, the ultimate IPv4 subnet allocation algorithm
	subnetBase := 0
	stepSize := 4

	// Walk subnets until a free slot is found and allocate it
	for {
		if subnetBase >= 255-stepSize {
			i.lock.Unlock()
			return errors.New("available VM address space exhausted")
		}

		if _, ok := i.ipamSlots[instanceGroup.MakeAddress(subnetBase)+"/30"]; !ok {
			break
		}

		subnetBase += 4
	}

	i.ipamSlots[instanceGroup.MakeAddress(subnetBase)+"/30"] = struct{}{}

	// Generate SSH key
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		i.lock.Unlock()
		return err
	}

	instanceIndex := subnetBase / stepSize
	instanceName := "fleetingd" + strconv.Itoa(instanceIndex)

	// Generate random mac address
	randomBytes := make([]byte, 4)
	_, err = rand.Read(randomBytes)
	if err != nil {
		i.lock.Unlock()
		return err
	}
	randomPart := hex.EncodeToString(randomBytes)

	// slicing like this is okay since it is an ASCII string
	instanceMac := fmt.Sprintf(
		"de:51:%s:%s:%s:%s",
		randomPart[0:2],
		randomPart[2:4],
		randomPart[4:6],
		randomPart[6:])

	hostTapIP := instanceGroup.MakeAddress(subnetBase + 1)
	instanceTapIP := instanceGroup.MakeAddress(subnetBase + 2)

	// Generate userdata image
	userdataPath, err := instanceGroup.createUserdata(instanceName,
		instanceMac,
		instanceTapIP,
		hostTapIP,
		"/30",
		pubKey)
	if err != nil {
		i.lock.Unlock()
		return err
	}

	// Create copy on write qcow image
	overlayPath, err := instanceGroup.createOverlay(instanceName)
	if err != nil {
		i.lock.Unlock()
		return err
	}

	kernelFilePath, err := instanceGroup.getKernelFilePath()
	if err != nil {
		i.lock.Unlock()
		return err
	}

	// Start instance
	instanceContext, instanceCancelFunc := context.WithCancel(context.Background())

	hypervisorCommand := exec.CommandContext(instanceContext, "cloud-hypervisor",
		"--kernel",
		kernelFilePath,
		"--disk",
		fmt.Sprintf("path=%s", overlayPath),
		fmt.Sprintf("path=%s,readonly=on", userdataPath),
		"--cpus",
		fmt.Sprintf("boot=%d", instanceGroup.VMNumCPUCores),
		"--memory",
		fmt.Sprintf("size=%dM", instanceGroup.VMMemoryMegabytes),
		"--net",
		fmt.Sprintf("tap=%s,mac=%s,ip=%s,mask=255.255.255.252", instanceName, instanceMac, hostTapIP),
		"--balloon",
		"size=0,free_page_reporting=on",
		"--cmdline",
		"console=hvc0 root=/dev/vda1 rw",
	)

	if instanceGroup.VMEnableVirtioConsole {
		// Enable console
		consolePath := filepath.Join(instanceGroup.VMDiskDir, vmWorkdir, fmt.Sprintf("%s_console", instanceName))

		hypervisorCommand.Args = append(hypervisorCommand.Args, "--console",
			fmt.Sprintf("file=%s", consolePath))
	}

	instanceGroup.logger.Info("starting instance VM", "instance", instanceName)
	hypervisorCommand.Start()

	go func() {
		//
		// VM cleanup - cancel VM context to trigger stopping the VM process and then calling this function
		//

		// Wait for VM to terminate (when context gets cancelled)
		hypervisorCommand.Wait()

		instanceGroup.logger.Info("instance process finished. cleaning up.", "instance", instanceName)

		// Delete overlay and cloudinit data
		err = os.Remove(overlayPath)
		if err != nil {
			instanceGroup.logger.Error("error deleting overlay after instance has been stopped: %w", err)
		}

		err = os.Remove(userdataPath)
		if err != nil {
			instanceGroup.logger.Error("error deleting userdata after instance has been stopped: %w", err)
		}

		i.lock.Lock()

		// Clear instance's IPAM lock
		delete(i.ipamSlots, instanceGroup.MakeAddress(subnetBase)+"/30")

		// Clear instance from inventory
		delete(i.instances, instanceName)

		i.lock.Unlock()

		i.ApplyNftables(instanceGroup)
	}()

	// Update inventory
	i.instances[instanceName] = &InstanceInfo{
		Name:                      instanceName,
		InstanceContextCancelFunc: instanceCancelFunc,

		HostTapIP:     hostTapIP,
		InstanceTapIP: instanceTapIP,

		InstanceTapMacAddress: instanceMac,

		SSHPublicKey:  pubKey,
		SSHPrivateKey: privKey,
	}

	// Release lock for nftables
	i.lock.Unlock()

	// Wait for tap device to become available
	checkCounter := 0
	tapReady := false

	for {
		interfaces, err := net.Interfaces()
		if err != nil {
			return err
		}
		for _, device := range interfaces {
			if device.Name == instanceName {
				tapReady = true
				break
			}
		}

		if tapReady || checkCounter > 100 {
			break
		}

		time.Sleep(100 * time.Millisecond)
		checkCounter++
	}

	// Render and apply nftables rules (wait for tap interface)
	return i.ApplyNftables(instanceGroup)
}

func (i *Inventory) PrebuildInstance(instanceGroup *InstanceGroup) error {
	i.lock.RLock()
	takenSlots := len(i.ipamSlots)
	i.lock.RUnlock()

	// Short-circuit function instead of walking adddress space
	if takenSlots >= MaxIPAMSlots {
		return errors.New("available VM address space exhausted")
	}

	i.lock.Lock()

	if i.shuttingDown {
		i.lock.Unlock()
		return errors.New("system is shutting down")
	}

	// Behold, the ultimate IPv4 subnet allocation algorithm
	subnetBase := 0
	stepSize := 4

	// Walk subnets until a free slot is found and allocate it
	for {
		if subnetBase >= 255-stepSize {
			i.lock.Unlock()
			return errors.New("available VM address space exhausted")
		}

		if _, ok := i.ipamSlots[instanceGroup.MakeAddress(subnetBase)+"/30"]; !ok {
			break
		}

		subnetBase += 4
	}

	i.ipamSlots[instanceGroup.MakeAddress(subnetBase)+"/30"] = struct{}{}

	instanceIndex := subnetBase / stepSize
	instanceName := "fleetingd" + strconv.Itoa(instanceIndex)

	// Generate random mac address
	randomBytes := make([]byte, 4)
	_, err := rand.Read(randomBytes)
	if err != nil {
		i.lock.Unlock()
		return err
	}
	randomPart := hex.EncodeToString(randomBytes)

	// slicing like this is okay since it is an ASCII string
	instanceMac := fmt.Sprintf(
		"de:51:%s:%s:%s:%s",
		randomPart[0:2],
		randomPart[2:4],
		randomPart[4:6],
		randomPart[6:])

	hostTapIP := instanceGroup.MakeAddress(subnetBase + 1)
	instanceTapIP := instanceGroup.MakeAddress(subnetBase + 2)

	// Generate userdata image
	userdataPath, err := instanceGroup.createUserdataPrebuild(instanceName,
		instanceMac,
		instanceTapIP,
		hostTapIP,
		"/30")
	if err != nil {
		i.lock.Unlock()
		return err
	}

	diskImageFileName, err := getFilenameFromURL(diskImageURL)
	if err != nil {
		return err
	}
	diskImageFilePath := filepath.Join(instanceGroup.VMDiskDir, diskImageFileName)
	decompressedPath := addSuffixToFilepath(diskImageFilePath, decompressedSuffix)

	kernelFilePath, err := instanceGroup.getKernelFilePath()
	if err != nil {
		i.lock.Unlock()
		return err
	}

	// Start instance
	instanceContext, instanceCancelFunc := context.WithCancel(context.Background())

	hypervisorCommand := exec.CommandContext(instanceContext, "cloud-hypervisor",
		"--kernel",
		kernelFilePath,
		"--disk",
		fmt.Sprintf("path=%s", decompressedPath),
		fmt.Sprintf("path=%s,readonly=on", userdataPath),
		"--cpus",
		fmt.Sprintf("boot=%d", instanceGroup.VMNumCPUCores),
		"--memory",
		fmt.Sprintf("size=%dM", instanceGroup.VMMemoryMegabytes),
		"--net",
		fmt.Sprintf("tap=%s,mac=%s,ip=%s,mask=255.255.255.252", instanceName, instanceMac, hostTapIP),
		"--balloon",
		"size=0,free_page_reporting=on",
		"--cmdline",
		"console=hvc0 root=/dev/vda1 rw")

	if instanceGroup.VMEnableVirtioConsole {
		// Enable console
		consolePath := filepath.Join(instanceGroup.VMDiskDir, vmWorkdir, fmt.Sprintf("%s_console", instanceName))

		hypervisorCommand.Args = append(hypervisorCommand.Args, "--console",
			fmt.Sprintf("file=%s", consolePath))
	}

	instanceGroup.logger.Info("starting instance VM", "instance", instanceName)
	hypervisorCommand.Start()
	prebuildDone := make(chan struct{})

	go func() {
		//
		// VM cleanup - cancel VM context to trigger stopping the VM process and then calling this function
		//

		// Wait for VM to terminate (when context gets cancelled)
		hypervisorCommand.Wait()

		instanceGroup.logger.Info("instance process finished. cleaning up.", "instance", instanceName)

		// Delete cloudinit data
		err = os.Remove(userdataPath)
		if err != nil {
			instanceGroup.logger.Error("error deleting userdata after instance has been stopped: %w", err)
		}

		i.lock.Lock()

		// Clear instance's IPAM lock
		delete(i.ipamSlots, instanceGroup.MakeAddress(subnetBase)+"/30")

		// Clear instance from inventory
		delete(i.instances, instanceName)

		i.lock.Unlock()

		i.ApplyNftables(instanceGroup)

		prebuildDone <- struct{}{}
	}()

	// Update inventory
	i.instances[instanceName] = &InstanceInfo{
		Name:                      instanceName,
		InstanceContextCancelFunc: instanceCancelFunc,

		HostTapIP:     hostTapIP,
		InstanceTapIP: instanceTapIP,

		InstanceTapMacAddress: instanceMac,

		SSHPublicKey:  nil,
		SSHPrivateKey: nil,
	}

	// Release lock for nftables
	i.lock.Unlock()

	// Wait for tap device to become available
	checkCounter := 0
	tapReady := false

	for {
		interfaces, err := net.Interfaces()
		if err != nil {
			return err
		}
		for _, device := range interfaces {
			if device.Name == instanceName {
				tapReady = true
				break
			}
		}

		if tapReady || checkCounter > 100 {
			break
		}

		time.Sleep(100 * time.Millisecond)
		checkCounter++
	}

	// Render and apply nftables rules (wait for tap interface)
	err = i.ApplyNftables(instanceGroup)
	if err != nil {
		return err
	}

	// Wait for prebuild to finish / cleanup
	instanceGroup.logger.Info("waiting for prebuild to finish.")
	<-prebuildDone
	instanceGroup.logger.Info("prebuild finished.")

	return nil
}

func (i *Inventory) DestroyInstance(name string) error {
	// Try to destroy an instance, return error if it did not work within 10 seconds

	i.lock.Lock()
	i.instances[name].InstanceContextCancelFunc()
	i.lock.Unlock()

	waitCounter := 0
	for {
		i.lock.RLock()
		_, instanceStillExists := i.instances[name]
		i.lock.RUnlock()

		if !instanceStillExists {
			return nil
		}

		waitCounter++
		if waitCounter > 100 {
			return fmt.Errorf("timed out waiting for instance %s to be removed", name)
		}

		time.Sleep(time.Millisecond * 100)
	}
}

func (i *Inventory) DestroyAllInstances() error {
	// Try to destroy all instances

	instanceNames := []string{}

	i.lock.Lock()
	// Block creation of new instances
	i.shuttingDown = true

	// Collect instance names to destroy
	for name, _ := range i.instances {
		instanceNames = append(instanceNames, name)
	}

	i.lock.Unlock()

	for _, instanceToDestroy := range instanceNames {
		err := i.DestroyInstance(instanceToDestroy)
		if err != nil {
			return err
		}
	}

	return nil
}

func (i *Inventory) GetAllInstances() []string {
	// List all instance names

	instanceNames := []string{}

	i.lock.RLock()

	for name, _ := range i.instances {
		instanceNames = append(instanceNames, name)
	}

	i.lock.RUnlock()

	return instanceNames
}

func (i *Inventory) GetConnectInfo(name string) (*provider.ConnectInfo, error) {
	// Get an instance's conneciton info

	i.lock.RLock()

	instance, ok := i.instances[name]
	if !ok {
		return nil, errors.New("instance not found")
	}

	marshalledKey, err := ssh.MarshalPrivateKey(instance.SSHPrivateKey, "fleetingd")
	if err != nil {
		return nil, err
	}

	connectionInfo := provider.ConnectInfo{
		ID:           instance.Name,
		InternalAddr: instance.InstanceTapIP,

		ConnectorConfig: provider.ConnectorConfig{
			Username: "ubuntu",
			OS:       "linux",
			Arch:     runtime.GOARCH,

			Protocol:     provider.ProtocolSSH,
			ProtocolPort: 22,
			Key:          pem.EncodeToMemory(marshalledKey),
			Keepalive:    time.Second * 10,
			Timeout:      time.Second * 3,
		},
	}

	i.lock.RUnlock()

	return &connectionInfo, nil
}

func (i *Inventory) ApplyNftables(instanceGroup *InstanceGroup) error {
	// Render nftables template for setup and apply it

	type nftablesTemplateInstanceInfo struct {
		Name                  string
		InstanceTapIP         string
		InstanceTapMacAddress string
		InstanceGateway       string
	}

	type nftablesTemplateArgs struct {
		EgressInterface string
		Instances       []nftablesTemplateInstanceInfo
	}

	templates, err := template.ParseFS(userDataTemplates, "templates/*.tpl")
	if err != nil {
		return err
	}

	templateArgs := nftablesTemplateArgs{
		EgressInterface: instanceGroup.EgressInterface,
		Instances:       []nftablesTemplateInstanceInfo{},
	}

	i.lock.RLock()
	for _, instance := range i.instances {
		templateArgs.Instances = append(templateArgs.Instances, nftablesTemplateInstanceInfo{
			Name:                  instance.Name,
			InstanceTapIP:         instance.InstanceTapIP,
			InstanceTapMacAddress: instance.InstanceTapMacAddress,
			InstanceGateway:       instance.HostTapIP,
		})
	}
	i.lock.RUnlock()

	rulesetPath := filepath.Join(instanceGroup.VMDiskDir, "ruleset.nft")

	rulesetFile, err := os.Create(rulesetPath)
	if err != nil {
		return err
	}
	defer rulesetFile.Close()

	err = templates.ExecuteTemplate(rulesetFile, "nftables-rules.tpl", templateArgs)
	if err != nil {
		return err
	}

	rulesetFile.Close()

	err = exec.Command("nft", "-f", rulesetPath).Run()
	if err != nil {
		return err
	}

	return nil
}
