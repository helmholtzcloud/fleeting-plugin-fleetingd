#cloud-config
package_update: true
package_upgrade: true
disable_root: true
ssh_pwauth: false
packages:
  - ufw
  - fail2ban
  - ca-certificates
  - curl
runcmd:
  # Mitigate Dirty Frag
  - echo "blacklist esp4" > /etc/modprobe.d/df-mitigation.conf
  - echo "blacklist esp6" >> /etc/modprobe.d/df-mitigation.conf
  - echo "blacklist rxrpc" >> /etc/modprobe.d/df-mitigation.conf
  - echo "install esp4 /bin/false" >> /etc/modprobe.d/df-mitigation.conf
  - echo "install esp6 /bin/false" >> /etc/modprobe.d/df-mitigation.conf
  - echo "install rxrpc /bin/false" >> /etc/modprobe.d/df-mitigation.conf
  - rmmod esp4 esp6 rxrpc 2>/dev/null

  # Firewall
  - ufw default deny incoming
  - ufw enable

  # fail2ban
  - systemctl enable fail2ban

  # Install latest GitLab runner so artifacts can be pulled
  - curl -L "https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.deb.sh" | os=ubuntu dist=noble bash
  - apt install -y gitlab-runner

  # CUSTOM COMMANDS START

{{range $command := .ExtraCommands}}
  - {{$command}}
{{end}}

  # CUSTOM COMMANDS END

  # Reset cloudinit so each machine gets a fresh SSH key
  - cloud-init clean --logs --machine-id --seed --configs all
  - shutdown -hP now