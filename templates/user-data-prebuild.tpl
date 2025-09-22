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
  # Firewall
  - ufw default deny incoming
  - ufw enable

  # fail2ban
  - systemctl enable fail2ban

  # Install latest GitLab runner so artifacts can be pulled
  - curl -L "https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.deb.sh" | sudo bash
  - apt install -y gitlab-runner

  # CUSTOM COMMANDS START

{{range $command := .ExtraCommands}}
  - {{$command}}
{{end}}

  # CUSTOM COMMANDS END

  # Reset cloudinit so each machine gets a fresh SSH key
  - cloud-init clean --logs --machine-id --seed --configs all
  - shutdown -hP now