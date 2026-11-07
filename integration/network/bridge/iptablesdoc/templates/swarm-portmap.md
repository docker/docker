## Swarm service, with a published port

Equivalent to:

	docker service create -p 8080:80 busybox top

The filter table is:

    {{index . "LFilter4"}}

<details>
<summary>iptables commands</summary>

    {{index . "SFilter4"}}

</details>

Note that:

 - There's a bridge network called `docker_gwbridge` for swarm ingress.
   - Its rules follow the usual pattern for a network with inter-container communication disabled.
 - The published port is set up as an ordinary port mapping on the ingress
   load-balancer sandbox's `docker_gwbridge` gateway endpoint (`172.18.0.2`),
   using the same rules as any other published container port:
   - a DNAT rule in the nat `DOCKER` chain. Unlike a container's port mapping
     when the userland proxy is running, it doesn't skip packets arriving on
     `docker_gwbridge`. The load-balancer never connects to its own published
     port, so a task that reaches the port via its node's address is DNATed
     like any other client, and doesn't go through docker-proxy, and
   - ACCEPT rules in the filter `DOCKER` chain. There are two, because
     `docker_gwbridge` has inter-container communication disabled: one for
     packets from outside the network, and one for packets from the network
     itself that reached the port via one of the host's addresses.
   - So, there's no separate `DOCKER-INGRESS` chain.

And the corresponding nat table:

    {{index . "LNat4"}}

<details>
<summary>iptables commands</summary>

    {{index . "SNat4"}}

</details>
