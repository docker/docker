<!-- This is a generated file; DO NOT EDIT. -->

## Swarm service, with a published port

Equivalent to:

	docker service create -p 8080:80 busybox top

The filter table is:

    Chain INPUT (policy ACCEPT 0 packets, 0 bytes)
    num   pkts bytes target     prot opt in     out     source               destination         
    
    Chain FORWARD (policy ACCEPT 0 packets, 0 bytes)
    num   pkts bytes target     prot opt in     out     source               destination         
    1        0     0 DOCKER-USER  all  --  any    any     anywhere             anywhere            
    2        0     0 DOCKER-FORWARD  all  --  any    any     anywhere             anywhere            
    
    Chain OUTPUT (policy ACCEPT 0 packets, 0 bytes)
    num   pkts bytes target     prot opt in     out     source               destination         
    
    Chain DOCKER (2 references)
    num   pkts bytes target     prot opt in     out     source               destination         
    1        0     0 ACCEPT     tcp  --  docker_gwbridge docker_gwbridge  anywhere             172.18.0.2           tcp dpt:http-alt ! ctorigdst 172.18.0.2
    2        0     0 ACCEPT     tcp  --  !docker_gwbridge docker_gwbridge  anywhere             172.18.0.2           tcp dpt:http-alt
    3        0     0 DROP       all  --  !docker0 docker0  anywhere             anywhere            
    4        0     0 DROP       all  --  !docker_gwbridge docker_gwbridge  anywhere             anywhere            
    
    Chain DOCKER-BRIDGE (1 references)
    num   pkts bytes target     prot opt in     out     source               destination         
    1        0     0 DOCKER     all  --  any    docker0  anywhere             anywhere            
    2        0     0 DOCKER     all  --  any    docker_gwbridge  anywhere             anywhere            
    
    Chain DOCKER-CT (1 references)
    num   pkts bytes target     prot opt in     out     source               destination         
    1        0     0 ACCEPT     all  --  any    docker0  anywhere             anywhere             ctstate RELATED,ESTABLISHED
    2        0     0 ACCEPT     all  --  any    docker_gwbridge  anywhere             anywhere             ctstate RELATED,ESTABLISHED
    
    Chain DOCKER-FORWARD (1 references)
    num   pkts bytes target     prot opt in     out     source               destination         
    1        0     0 DOCKER-CT  all  --  any    any     anywhere             anywhere            
    2        0     0 DOCKER-INTERNAL  all  --  any    any     anywhere             anywhere            
    3        0     0 DOCKER-BRIDGE  all  --  any    any     anywhere             anywhere            
    4        0     0 ACCEPT     all  --  docker0 any     anywhere             anywhere            
    5        0     0 DROP       all  --  docker_gwbridge docker_gwbridge  anywhere             anywhere            
    6        0     0 ACCEPT     all  --  docker_gwbridge !docker_gwbridge  anywhere             anywhere            
    
    Chain DOCKER-INTERNAL (1 references)
    num   pkts bytes target     prot opt in     out     source               destination         
    
    Chain DOCKER-USER (1 references)
    num   pkts bytes target     prot opt in     out     source               destination         
    

<details>
<summary>iptables commands</summary>

    -P INPUT ACCEPT
    -P FORWARD ACCEPT
    -P OUTPUT ACCEPT
    -N DOCKER
    -N DOCKER-BRIDGE
    -N DOCKER-CT
    -N DOCKER-FORWARD
    -N DOCKER-INTERNAL
    -N DOCKER-USER
    -A FORWARD -j DOCKER-USER
    -A FORWARD -j DOCKER-FORWARD
    -A DOCKER -d 172.18.0.2/32 -i docker_gwbridge -o docker_gwbridge -p tcp -m tcp --dport 8080 -m conntrack ! --ctorigdst 172.18.0.2 -j ACCEPT
    -A DOCKER -d 172.18.0.2/32 ! -i docker_gwbridge -o docker_gwbridge -p tcp -m tcp --dport 8080 -j ACCEPT
    -A DOCKER ! -i docker0 -o docker0 -j DROP
    -A DOCKER ! -i docker_gwbridge -o docker_gwbridge -j DROP
    -A DOCKER-BRIDGE -o docker0 -j DOCKER
    -A DOCKER-BRIDGE -o docker_gwbridge -j DOCKER
    -A DOCKER-CT -o docker0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
    -A DOCKER-CT -o docker_gwbridge -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
    -A DOCKER-FORWARD -j DOCKER-CT
    -A DOCKER-FORWARD -j DOCKER-INTERNAL
    -A DOCKER-FORWARD -j DOCKER-BRIDGE
    -A DOCKER-FORWARD -i docker0 -j ACCEPT
    -A DOCKER-FORWARD -i docker_gwbridge -o docker_gwbridge -j DROP
    -A DOCKER-FORWARD -i docker_gwbridge ! -o docker_gwbridge -j ACCEPT
    

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

    Chain PREROUTING (policy ACCEPT 0 packets, 0 bytes)
    num   pkts bytes target     prot opt in     out     source               destination         
    1        0     0 DOCKER     all  --  any    any     anywhere             anywhere             ADDRTYPE match dst-type LOCAL
    
    Chain INPUT (policy ACCEPT 0 packets, 0 bytes)
    num   pkts bytes target     prot opt in     out     source               destination         
    
    Chain OUTPUT (policy ACCEPT 0 packets, 0 bytes)
    num   pkts bytes target     prot opt in     out     source               destination         
    1        0     0 DOCKER     all  --  any    any     anywhere            !loopback/8           ADDRTYPE match dst-type LOCAL
    
    Chain POSTROUTING (policy ACCEPT 0 packets, 0 bytes)
    num   pkts bytes target     prot opt in     out     source               destination         
    1        0     0 MASQUERADE  all  --  any    !docker_gwbridge  172.18.0.0/16        anywhere            
    2        0     0 MASQUERADE  all  --  any    !docker0  172.17.0.0/16        anywhere            
    
    Chain DOCKER (2 references)
    num   pkts bytes target     prot opt in     out     source               destination         
    1        0     0 DNAT       tcp  --  any    any     anywhere             anywhere             tcp dpt:http-alt to:172.18.0.2:8080
    

<details>
<summary>iptables commands</summary>

    -P PREROUTING ACCEPT
    -P INPUT ACCEPT
    -P OUTPUT ACCEPT
    -P POSTROUTING ACCEPT
    -N DOCKER
    -A PREROUTING -m addrtype --dst-type LOCAL -j DOCKER
    -A OUTPUT ! -d 127.0.0.0/8 -m addrtype --dst-type LOCAL -j DOCKER
    -A POSTROUTING -s 172.18.0.0/16 ! -o docker_gwbridge -j MASQUERADE
    -A POSTROUTING -s 172.17.0.0/16 ! -o docker0 -j MASQUERADE
    -A DOCKER -p tcp -m tcp --dport 8080 -j DNAT --to-destination 172.18.0.2:8080
    

</details>
