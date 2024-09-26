# High-Availability Flow

This is a tool to provide automatic fail-over in combination with [keepalived](https://www.keepalived.org/) on
the [Flow Swiss](https://flow.swiss/) cloud.

## Limitations

Floating IPs cannot be used across regions on Flow Swiss therefore, this only works for instances in a private network
on the same region.

## Example

This example will use NGINX and keepalived.

1. Create 2< compute instances in the same private network
2. Install everything: `sudo apt install -y nginx keepalived`
3. Set up the fail-over script in `/etc/keepalived/failover.sh`

```shell
#!/bin/bash
# replace these values
HA_EIP=<YOUR HIGH AVAILABILITY ELASTIC IP>
INSTANCE_IDS=<THE IDS OF ALL PEERS>
TOKEN=<FLOW API TOKEN>

/opt/ha-flow -eip=$HA_EIP -instance_ids=$INSTANCE_IDS -token=$TOKEN 
```

4. Set up a master/backup configuration using
   On the master add the following to `/etc/keepalived/keepalived.conf`

```text
global_defs {
    router_id node01                # set hostname
}

vrrp_script chk_nginx {
    script "/usr/bin/pgrep nginx"
    interval 2
}

vrrp_instance VRRP1 {
    state MASTER
    interface ens4                  # ip addr show
    virtual_router_id 101
    priority 200                    # MASTER > BACKUP
    advert_int 1                    # VRRP advertisement interval (sec)
    unicast_src_ip 172.31.0.10      # private IP of master
    unicast_peer {
        172.31.0.11                 # private IP of backup
    }
    authentication {
        auth_type PASS
        auth_pass [random_password] # replace with a securely generated secret
    }
    
    track_script {
        chk_nginx                   # script to check if local nginx is alive
    }
    
    notify_master /etc/keepalived/failover.sh
}
```

On the backup add the following to `/etc/keepalived/keepalived.conf`

```text
global_defs {
    router_id node02
}

vrrp_script chk_nginx {
    script "/usr/bin/pgrep nginx"
    interval 2
}

vrrp_instance VRRP1 {
    state BACKUP 
    interface ens4                  # ip addr show
    virtual_router_id 101
    priority 100                    # MASTER > BACKUP
    advert_int 1                    # VRRP advertisement interval (sec)
    unicast_src_ip 172.31.0.11      # private IP of backup
    unicast_peer {
        172.31.0.10                 # private IP of master
    }
    authentication {
        auth_type PASS
        auth_pass [random_password] # replace with a securely generated secret (must be same as master)
    }
    
    track_script {
        chk_nginx                   # script to check if local nginx is alive
    }
    
    notify_master /etc/keepalived/failover.sh
}
```

If you want the hosts to share a local virtual IP address you can add the following in `vrrp_instance` block of
the `keepalive.conf`

```text
virtual_ipaddress {
    172.31.0.100/16                 # virtual IP CIDR that will be shared, both instances will accept packets from that IP
}
```

### Sharing configurations across instances

You might want to set up `lsync` or `rsync` with `ssh` so that configuration changes pushed to the master are
automatically propagated to peers.
