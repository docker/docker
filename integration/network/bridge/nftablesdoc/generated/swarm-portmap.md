<!-- This is a generated file; DO NOT EDIT. -->

## Swarm service, with a published port

Equivalent to:

	docker service create -p 8080:80 busybox top

The `ip docker-bridges` table is:

    table ip docker-bridges {
    	map filter-forward-in-jumps {
    		type ifname : verdict
    		elements = { "docker0" : jump filter-forward-in__docker0,
    			     "docker_gwbridge" : jump filter-forward-in__docker_gwbridge }
    	}
    
    	map filter-forward-out-jumps {
    		type ifname : verdict
    		elements = { "docker0" : jump filter-forward-out__docker0,
    			     "docker_gwbridge" : jump filter-forward-out__docker_gwbridge }
    	}
    
    	map nat-postrouting-in-jumps {
    		type ifname : verdict
    		elements = { "docker0" : jump nat-postrouting-in__docker0,
    			     "docker_gwbridge" : jump nat-postrouting-in__docker_gwbridge }
    	}
    
    	map nat-postrouting-out-jumps {
    		type ifname : verdict
    		elements = { "docker0" : jump nat-postrouting-out__docker0,
    			     "docker_gwbridge" : jump nat-postrouting-out__docker_gwbridge }
    	}
    
    	chain filter-FORWARD {
    		type filter hook forward priority filter; policy accept;
    		oifname vmap @filter-forward-in-jumps
    		iifname vmap @filter-forward-out-jumps
    	}
    
    	chain nat-OUTPUT {
    		type nat hook output priority dstnat; policy accept;
    		ip daddr != 127.0.0.0/8 fib daddr type local counter jump nat-prerouting-and-output
    	}
    
    	chain nat-POSTROUTING {
    		type nat hook postrouting priority srcnat; policy accept;
    		iifname vmap @nat-postrouting-out-jumps
    		oifname vmap @nat-postrouting-in-jumps
    	}
    
    	chain nat-PREROUTING {
    		type nat hook prerouting priority dstnat; policy accept;
    		fib daddr type local counter jump nat-prerouting-and-output
    	}
    
    	chain nat-prerouting-and-output {
    		tcp dport 8080 counter dnat to 172.18.0.2:8080 comment "DNAT"
    	}
    
    	chain raw-PREROUTING {
    		type filter hook prerouting priority raw; policy accept;
    		ip daddr 172.18.0.2 iifname != "docker_gwbridge" counter drop comment "DROP DIRECT ACCESS"
    		ip daddr 172.18.0.3 iifname != "docker_gwbridge" counter drop comment "DROP DIRECT ACCESS"
    	}
    
    	chain filter-forward-in__docker0 {
    		ct state established,related counter accept
    		iifname "docker0" counter accept comment "ICC"
    		counter drop comment "UNPUBLISHED PORT DROP"
    	}
    
    	chain filter-forward-out__docker0 {
    		ct state established,related counter accept
    		counter accept comment "OUTGOING"
    	}
    
    	chain nat-postrouting-in__docker0 {
    	}
    
    	chain nat-postrouting-out__docker0 {
    		oifname != "docker0" ip saddr 172.17.0.0/16 counter masquerade comment "MASQUERADE"
    	}
    
    	chain filter-forward-in__docker_gwbridge {
    		ct state established,related counter accept
    		iifname "docker_gwbridge" ip daddr 172.18.0.2 tcp dport 8080 ct original ip daddr != 172.18.0.2 counter accept
    		iifname "docker_gwbridge" counter drop comment "ICC"
    		ip daddr 172.18.0.2 tcp dport 8080 counter accept
    		counter drop comment "UNPUBLISHED PORT DROP"
    	}
    
    	chain filter-forward-out__docker_gwbridge {
    		ct state established,related counter accept
    		counter accept comment "OUTGOING"
    	}
    
    	chain nat-postrouting-in__docker_gwbridge {
    	}
    
    	chain nat-postrouting-out__docker_gwbridge {
    		oifname != "docker_gwbridge" ip saddr 172.18.0.0/16 counter masquerade comment "MASQUERADE"
    	}
    }
    

Note that:

 - There's a bridge network called `docker_gwbridge` for swarm ingress.
   - Its rules follow the usual pattern for a network with inter-container communication disabled.
 - The published port is set up as an ordinary port mapping on the ingress
   load-balancer sandbox's `docker_gwbridge` gateway endpoint (`172.18.0.2`),
   using the same rules as any other published container port:
   - a DNAT rule in chain `nat-prerouting-and-output`. Unlike a container's port
     mapping when the userland proxy is running, it doesn't skip packets arriving
     on `docker_gwbridge`. The load-balancer never connects to its own published
     port, so a task that reaches the port via its node's address is DNATed like
     any other client, and doesn't go through docker-proxy, and
   - accept rules in chain `filter-forward-in__docker_gwbridge`. There are two,
     because `docker_gwbridge` has inter-container communication disabled: one
     for packets from outside the network, and one for packets from the network
     itself that reached the port via one of the host's addresses.
 - The task's `docker_gwbridge` endpoint (`172.18.0.3`) has no published ports,
   but chain `raw-PREROUTING` drops direct access to it from outside the network.

The ingress load-balancer sandbox (`ingress_sbox`) has its own network namespace.
Its `ip docker-lb-nat` table is:

    table ip docker-lb-nat {
    	map nat-publish-port {
    		typeof meta l4proto . th dport . numgen random mod 65536 : ip daddr
    		flags interval
    		counter
    		elements = { tcp . 8080 . 0-65535 counter : 10.0.0.4 }
    	}
    
    	map nat-service-vip {
    		typeof ip daddr . numgen random mod 65536 : ip daddr
    		flags interval
    		counter
    		elements = { 10.0.0.3 . 0-65535 counter : 10.0.0.4 }
    	}
    
    	chain postrouting {
    		type nat hook postrouting priority srcnat; policy accept;
    		ct status dnat counter masquerade
    	}
    
    	chain prerouting {
    		type nat hook prerouting priority dstnat; policy accept;
    		dnat to ip daddr . numgen random mod 65536 map @nat-service-vip
    		dnat to meta l4proto . th dport . numgen random mod 65536 map @nat-publish-port
    	}
    }

In this table:

 - Map `nat-publish-port` selects a backend for each new connection to a
   published port. The key is the port's protocol and number, and a random
   number in the range 0-65535. Each of the service's tasks gets an equal share
   of that range. Here, there's one task, with address `10.0.0.4` on the ingress
   network. The destination port is not changed.
 - Map `nat-service-vip` does the same for the service's virtual IP address on
   the ingress network (`10.0.0.3`).
 - Chain `postrouting` masquerades DNAT'd connections, so the task's replies
   return through the sandbox.
 - The DNAT'd packets are routed to the task, so IPv4 forwarding is enabled in
   the sandbox's network namespace.

The task's network namespace has an `ip docker-container-ingress` table:

    table ip docker-container-ingress {
    	map published-ports {
    		type ipv4_addr . inet_proto . inet_service : inet_service
    		elements = { 10.0.0.4 . tcp . 8080 : 80 }
    	}
    
    	set ingress-ips {
    		type ipv4_addr
    		elements = { 10.0.0.4 }
    	}
    
    	chain input {
    		type filter hook input priority filter; policy accept;
    		ip daddr != @ingress-ips counter accept
    		icmp type { destination-unreachable, time-exceeded } counter accept
    		ct state { established, related } counter accept
    		ct status dnat counter accept
    		counter reject
    	}
    
    	chain output {
    		type filter hook output priority filter; policy accept;
    		ip saddr != @ingress-ips counter accept
    		icmp type { destination-unreachable, time-exceeded } counter accept
    		ct status dnat ct state { established, related } counter accept
    		counter reject
    	}
    
    	chain prerouting {
    		type nat hook prerouting priority dstnat; policy accept;
    		meta l4proto { tcp, udp, sctp } redirect to :ip daddr . meta l4proto . th dport map @published-ports
    	}
    }

In this table:

 - Chain `prerouting` redirects the published port on the task's ingress
   address to the target port, `8080` to `80`.
 - Chain `input` rejects new connections to the task's ingress address, unless
   they have been redirected from a published port.
 - Chain `output` rejects packets from the task's ingress address, other than
   replies on redirected connections.
 - In both chains, packets for the task's other addresses, and ICMP errors,
   are accepted.
