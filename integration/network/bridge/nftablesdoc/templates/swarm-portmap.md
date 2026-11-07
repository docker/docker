## Swarm service, with a published port

Equivalent to:

	docker service create -p 8080:80 busybox top

The `ip docker-bridges` table is:

{{index . "Ruleset4"}}

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

{{index . "IngressLB4"}}

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

{{index . "Task0Ingress4"}}

In this table:

 - Chain `prerouting` redirects the published port on the task's ingress
   address to the target port, `8080` to `80`.
 - Chain `input` rejects new connections to the task's ingress address, unless
   they have been redirected from a published port.
 - Chain `output` rejects packets from the task's ingress address, other than
   replies on redirected connections.
 - In both chains, packets for the task's other addresses, and ICMP errors,
   are accepted.
