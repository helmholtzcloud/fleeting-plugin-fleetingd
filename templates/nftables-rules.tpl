table ip fleetingdforwarding;
delete table ip fleetingdforwarding;
{{ if .Instances }}
table ip fleetingdforwarding {
  chain dropnottap {
    type filter hook forward priority 0; policy drop;

{{ range $instance := .Instances }}
    iifname "{{ $.EgressInterface }}" oifname "{{ $instance.Name }}" counter accept;
    iifname "{{ $instance.Name }}" oifname "{{ $.EgressInterface }}" counter accept;
{{ end }}
  }
}
{{ end }}

table netdev fleetingdfilter;
delete table netdev fleetingdfilter;
{{ if .Instances }}
table netdev fleetingdfilter {
{{ range $instance := .Instances }}
  chain {{ $instance.Name }} {
    type filter hook ingress device "{{ $instance.Name }}" priority 0; policy accept;

    ether saddr != "{{ $instance.InstanceTapMacAddress }}" counter drop;
    ip saddr != {{ $instance.InstanceTapIP }} counter drop;

    ip daddr {{ $instance.InstanceGateway }} counter accept;
    ip daddr 172.16.120.0/24 counter drop;
  }
{{ end }}
}
{{ end }}

table ip fleetingdsnat;
delete table ip fleetingdsnat;
{{ if .Instances }}
table ip fleetingdsnat {
  chain taptonet {
    type nat hook postrouting priority 100;

{{ range $instance := .Instances }}
    iifname {{ $instance.Name }} oifname "{{ $.EgressInterface }}" counter masquerade fully-random;
{{ end }}
  }
}
{{ end }}