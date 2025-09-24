# fleeting-plugin-fleetingd - Ephemeral VMs for GitLab CI

This is a [fleeting plugin](https://docs.gitlab.com/runner/fleet_scaling/fleeting/) for running a pool of customizable disposable VMs for executing GitLab CI tasks. Whereas other plugins maintain a pool on a cloud provider's platform you can bring your own (rental) bare-metal hardware here.
The original motivation was to provide users with a simple way to build container images, but it can be used to run anything else you would run in VMs or where VM-level isolation is desired for GitLab builds. Users receive a new Ubuntu machine with passwordless sudo for each build and a selection of software of your choice preinstalled (e.g. Podman). Altenatively, you can switch it to `docker-autoscaler` and execute builds and services running in containers in the VMs.

Note that there are [rootless alternatives](https://docs.gitlab.com/ci/docker/using_docker_build/#docker-alternatives) for building images but you might not always be able to configure the environment to accommodate them.

## Is it good?

While the basic use case works and has been tested in multiple projects, this software should be considered early-stage. There are some areas to rework (looking at you nftables SNAT). In addition, more data on container image build performance is needed to determine whether it is viable to continue the project.

## Implementation Overview

Quick implementation rundown: this runner works by downloading the latest Ubuntu Cloud LTS image + kernel to the local cache (`vm_disk_directory`), resizing it to the desired size (sparse allocation), launching a [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) VM (direct kernel boot) and applying a customizable base `cloud-init` script to the prebuild (see `templates/user-data-prebuild.tpl` for details). Then, the runner maintains pool of cloned worker machines (disks are cloned in copy-on-write fashion) for running builds. Depending on your hardware the runner should be able to spin-up new machines within a few seconds. Machines are controlled through SSH by the runner process and connect to the network through a TAP interface that is SNAT'ed using `nftables` rules on the host. Machines can be discarded after each build so that each build reveives a new VM.

## Example Setup on Ubuntu

Example testing setup on Ubuntu 22.04 LTS: this modifies the network config such as IPv4 forwarding and enabling nftables to get SNAT network access for the VMs. Check whether this is compatible with your firewall/network setup!

- Do the e.g. user / security / unattended-upgrades configuration for a regular Ubuntu Server host, including e.g. using nftables for your firewall and adding a basic ruleset blocking incoming connections
- Enable IP forwarding in `/etc/sysctl.conf` (`net.ipv4.ip_forward=1`)
- [Add Cloud Hypervisor apt source](https://github.com/cloud-hypervisor/obs-packaging)
- `sudo apt install cloud-hypervisor qemu-utils`
- [Install gitlab runner](https://docs.gitlab.com/runner/install/linux-repository/)
- Download the latest plugin binary from this repo's releases and place it in `/usr/local/bin`
- Edit runner config at `/etc/gitlab-runner/config.toml` (see [Configuration Reference](#configuration-reference) below)
- Enable nftables service and start it
  - Configure nftables to your liking (e.g. basic firewall setup, just pay attention to not fiddle with forwarding too much or lock yourself out by blocking SSH)
  - `sudo systemctl enable nftables`
  - `sudo systemctl start nftables`
- Modify the runner systemd service:
    - `sudo nano /etc/systemd/system/gitlab-runner.service`
      - Network must be online so that images can be downloaded properly
          - In `[Unit]` set `After=network-online.target`
      - Auto-restart every 24h to rebuild/fetch base image
          - In `[Service]` set `RuntimeMaxSec=24h`
    - `sudo systemctl daemon-reload`
    - `sudo systemctl restart gitlab-runner`
- Reboot the host
- `sudo journalctl -u gitlab-runner` should show the machine(s) booting

### Tweaks

#### VM disks on tmpfs

If you have spare memory you may run the the VM disks on a tmpfs:

- Modify `/etc/fstab` - careful here ;)
- Add a tmpfs mount with e.g. 30% of the available memory: `tmpfs /runnervms tmpfs size=30%,uid=0,gid=0,user,mode=0700,noatime 0 0`
- Reboot and hope for the best

#### Install Docker and Podman

For container builds you can add this to add Docker and Podman to the VMs:

```toml
[[runners]]
  [runners.autoscaler.plugin_config]
    vm_prebuild_cloudinit_extra_cmds = [
      # Install podman
      'apt install -y podman',

      # Install docker, add ubuntu to group
      'curl -fsSL https://get.docker.com -o get-docker.sh',
      'sh get-docker.sh',
      'usermod -aG docker ubuntu',
      '"echo \"{ \\\"features\\\": { \\\"buildkit\\\": true }, \\\"features\\\": { \\\"containerd-snapshotter\\\": true } }\" | tee -a /etc/docker/daemon.json"',
    ]
```

#### Pre-authenticate GitLab CI Container Registry

As a convenience feature for your users you can pre-authenticate the GitLab container registry in `/etc/gitlab-runner/config.toml`:

```toml
[[runners]]
  pre_build_script = 'echo $CI_REGISTRY_PASSWORD | docker login -u $CI_REGISTRY_USER --password-stdin $CI_REGISTRY'
```

Both Podman and Docker can use this config, enabling `podman push ...` etc. without any extra steps.

### Troubleshooting

#### Gitlab runner is stuck at waiting for prebuild
This is most probably either the networking setup or some issue with the provided `cloud-init` commands:

##### Checking the console
You can temporarily set `vm_enable_virtio_console` to `true`, restart the runner and check the VM logs (prebuild is always `fleetingd0`) in the `vm_disk_directory`, for example with `less -r /tmp/fleetingd/.instance_data/fleetingd0_console`.

##### Debugging networking
Check `nft list ruleset`. You should see counters above `0` in the `dropnottap` chain's `accept` rules of `fleetingd0` (the prebuild machine). Maybe you misspelled the egress interface name in the config.

### Limitations
- At this time there is no OCI release distribution. For now, you'll have to download the binaries from the latest release. While OCI distribution is worked on you may subscribe to the [release feed](https://github.com/helmholtzcloud/fleeting-plugin-fleetingd/releases.atom) in the meantime.
- As-is this relies on a bunch of rootful commands (e.g. modifying nftables). In the future this functionality could be better spearated.
- Currently only Ubuntu Cloud LTS is supported. Support could also be expanded to other `user-data`-provisionable distributions.
- The `nftables` SNAT mechanism is a bit barebones to say the least, also the use of `/30`s for allocating VM IPs could be more elegant e.g. by utilizing a OVN-backed approach.

### Configuration Reference

This is the subset of `gitlab-runner` config relevant for this project. When setting up a new instance these settings should be customized (see comments below for docs):

- `max_instances`
- `idle_count`
- `egress_interface`
- `vm_disk_directory`
- `vm_num_cpu_cores`
- `vm_memory_mb`
- `vm_disk_size_gb`
- `vm_prebuild_cloudinit_extra_cmds`

Ensure `vm_subnet` does not overlap with your LAN to avoid routing trouble.

You can run `fleeting-plugin-fleetingd licenses` to view the software's and dependency licenses.


```toml
[[runners]]
  # Treat VMs as simple instance runners
  # Switch to docker-autoscaler for running containerized jobs inside the VM
  executor = "instance"

  # Use bash shell
  shell = "bash"

  # Select the correct plugin
  # Download the latest release binary and place it in /usr/local/bin
  plugin = "fleeting-plugin-fleetingd"

  # Machines are initialized with cloud-init, wait for it to finish
  instance_ready_command = "cloud-init status --wait"

  # Shut down all VM instances when the runner is restarted
  delete_instances_on_shutdown = true

  [runners.autoscaler]
    # Run only a single task per VM
    capacity_per_instance = 1

    # Discard the VM after one run
    max_use_count = 1

    # Tunable: Run up to 10 VMs in parallel
    max_instances = 10

    # Connect using the ubuntu user
    [runners.autoscaler.connector_config]
      username = "ubuntu"
      keepalive = "1s"
      timeout = "10s"

    [[runners.autoscaler.policy]]
      # Keep 4 idle instances around so if a pipeline launches multiple jobs they are picked-up relatively fast
      idle_count = 4

      # Scale down if pool grew too large
      idle_time = "5m0s"

      # Launch new VMs as needed
      preemtive_mode = true

    #
    # Plugin configuration
    #

    [runners.autoscaler.plugin_config]
      # The VMs are going to use this interface for gress traffic / internet access
      egress_interface = "eth0"

      # The directory where OS images, kernel images and the VM's ephemeral disks are stored
      vm_disk_directory = "/tmp/fleetingd"

      # The subnet the VMs are going to be attached to
      vm_subnet = "172.16.120."

      # Number of vCPU cores available per VM
      vm_num_cpu_cores = 8

      # RAM per VM instance (ballooning is enabled so you may overcommit depending on your use case)
      vm_memory_mb = 16384

      # VM disk size in GB (sparse allocation)
      vm_disk_size_gb = 30

      # Inject some extra cloudinit commands to run during prebuild, add your VM image customization here:
      vm_prebuild_cloudinit_extra_cmds = [
        # Example: This is run as root
        'apt install -y --no-install-recommends git build-essential',

        # Example: This is run as ubuntu
        'su - ubuntu -c "whoami"',
      ]

      # You can enable the virtio console file in the .instance_data subdirectory of vm_disk_directory
      vm_enable_virtio_console = false
```