network:
  version: 2
  renderer: networkd
  ethernets:
    veth0:
      match:
        macaddress: {{ .MACAddress }}
      set-name: veth0
      dhcp4: false
      dhcp6: false
      mtu: 1500
      addresses:
        - {{ .IP }}{{ .Netmask }}
      routes:
        - to: default
          via: {{ .Gateway }}
      nameservers:
        addresses: [1.1.1.3, 1.0.0.3]