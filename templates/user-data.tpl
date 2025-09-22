#cloud-config
hostname: {{ .InstanceName }}
package_update: true
package_upgrade: true
disable_root: true
ssh_pwauth: false
ssh_authorized_keys:
  - "{{ .SSHAuthorizedPublicKey }}"
runcmd:
  - ufw allow from {{ .Gateway }} proto tcp to any port 22