---
keywords: "API, Usage, plugins, documentation, developer"
title: Plugins and Services
---

<!-- This file is maintained within the docker/cli GitHub
     repository at https://github.com/docker/cli/. Make all
     pull requests against that repo. If you see this file in
     another repository, consider it read-only there, as it will
     periodically be overwritten by the definitive file. Pull
     requests which include edits to this file in other repositories
     will be rejected.
-->

# Using Volume and Network plugins in Docker services

In swarm mode, it is possible to create a service that allows for attaching
to networks or mounting volumes that are backed by plugins. Swarm schedules
services based on plugin availability on a node.


### Volume plugins

In this example, a volume plugin is installed on a swarm worker and a volume
is created using the plugin. In the manager, a service is created with the
relevant mount options. It can be observed that the service is scheduled to
run on the worker node with the said volume plugin and volume. Note that,
node1 is the manager and node2 is the worker.

1.  Prepare manager. In node 1:

    ```bash
    $ docker swarm init
    Swarm initialized: current node (dxn1zf6l61qsb1josjja83ngz) is now a manager.
    ```

2. Join swarm, install plugin and create volume on worker. In node 2:

    ```bash
    $ docker swarm join \
    --token SWMTKN-1-49nj1cmql0jkz5s954yi3oex3nedyz0fb0xx14ie39trti4wxv-8vxv8rssmk743ojnwacrr2e7c \
    192.168.99.100:2377
    ```

    ```bash
    $ docker plugin install tiborvass/sample-volume-plugin
    latest: Pulling from tiborvass/sample-volume-plugin
    eb9c16fbdc53: Download complete
    Digest: sha256:00b42de88f3a3e0342e7b35fa62394b0a9ceb54d37f4c50be5d3167899994639
    Status: Downloaded newer image for tiborvass/sample-volume-plugin:latest
    Installed plugin tiborvass/sample-volume-plugin
    ```

    ```bash
    $ docker volume create -d tiborvass/sample-volume-plugin --name pluginVol
    ```

3. Create a service using the plugin and volume. In node1:

    ```bash
    $ docker service create --name my-service --mount type=volume,volume-driver=tiborvass/sample-volume-plugin,source=pluginVol,destination=/tmp busybox top

    $ docker service ls
    z1sj8bb8jnfn  my-service   replicated  1/1       busybox:latest
    ```
    docker service ls shows service 1 instance of service running.

4. Observe the task getting scheduled in node 2:

    ```bash
    {% raw %}
    $ docker ps --format '{{.ID}}\t {{.Status}} {{.Names}} {{.Command}}'
    83fc1e842599     Up 2 days my-service.1.9jn59qzn7nbc3m0zt1hij12xs "top"
    {% endraw %}
    ```

### Network plugins

In this example, a global scope network plugin is installed on both the
swarm manager and worker. A service is created with replicated instances
using the installed plugin. We will observe how the availability of the
plugin determines network creation and container scheduling.

Note that node1 is the manager and node2 is the worker.


1. Install a global scoped network plugin on both manager and worker. On node1
   and node2:

    ```bash
    $ docker plugin install bboreham/weave2
    Plugin "bboreham/weave2" is requesting the following privileges:
    - network: [host]
    - capabilities: [CAP_SYS_ADMIN CAP_NET_ADMIN]
    Do you grant the above permissions? [y/N] y
    latest: Pulling from bboreham/weave2
    7718f575adf7: Download complete
    Digest: sha256:2780330cc15644b60809637ee8bd68b4c85c893d973cb17f2981aabfadfb6d72
    Status: Downloaded newer image for bboreham/weave2:latest
    Installed plugin bboreham/weave2
    ```

2. Create a network using plugin on manager. On node1:

    ```bash
    $ docker network create --driver=bboreham/weave2:latest globalnet

    $ docker network ls
    NETWORK ID          NAME                DRIVER                   SCOPE
    qlj7ueteg6ly        globalnet           bboreham/weave2:latest   swarm
    ```

3. Create a service on the manager and have replicas set to 8. Observe that
containers get scheduled on both manager and worker.

    On node 1:

    ```bash
    $ docker service create --network globalnet --name myservice --replicas=8 mrjana/simpleweb simpleweb
w90drnfzw85nygbie9kb89vpa
    ```

    ```bash
    $ docker ps
    CONTAINER ID        IMAGE                                                                                      COMMAND             CREATED             STATUS              PORTS               NAMES
    87520965206a        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         5 seconds ago       Up 4 seconds                            myservice.4.ytdzpktmwor82zjxkh118uf1v
    15e24de0f7aa        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         5 seconds ago       Up 4 seconds                            myservice.2.kh7a9n3iauq759q9mtxyfs9hp
    c8c8f0144cdc        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         5 seconds ago       Up 4 seconds                            myservice.6.sjhpj5gr3xt33e3u2jycoj195
    2e8e4b2c5c08        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         5 seconds ago       Up 4 seconds                            myservice.8.2z29zowsghx66u2velublwmrh
    ```

    On node 2:

    ```bash
    $ docker ps
    CONTAINER ID        IMAGE                                                                                      COMMAND             CREATED             STATUS                  PORTS               NAMES
    53c0ae7c1dae        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         2 seconds ago       Up Less than a second                       myservice.7.x44tvvdm3iwkt9kif35f7ykz1
    9b56c627fee0        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         2 seconds ago       Up Less than a second                       myservice.1.x7n1rm6lltw5gja3ueikze57q
    d4f5927ba52c        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         2 seconds ago       Up 1 second                                 myservice.5.i97bfo9uc6oe42lymafs9rz6k
    478c0d395bd7        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         2 seconds ago       Up Less than a second                       myservice.3.yr7nkffa48lff1vrl2r1m1ucs
    ```

4. Scale down the number of instances. On node1:

    ```bash
    $ docker service scale myservice=0
    myservice scaled to 0
    ```

5. Disable and uninstall the plugin on the worker. On node2:

    ```bash
    $ docker plugin rm -f bboreham/weave2
    bboreham/weave2
    ```

6. Scale up the number of instances again. Observe that all containers are
scheduled on the master and not on the worker, because the plugin is not available on the worker anymore.

    On node 1:

    ```bash
    $ docker service scale myservice=8
    myservice scaled to 8
    ```

    ```bash
    $ docker ps
    CONTAINER ID        IMAGE                                                                                      COMMAND             CREATED             STATUS              PORTS               NAMES
    cf4b0ec2415e        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         39 seconds ago      Up 36 seconds                           myservice.3.r7p5o208jmlzpcbm2ytl3q6n1
    57c64a6a2b88        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         39 seconds ago      Up 36 seconds                           myservice.4.dwoezsbb02ccstkhlqjy2xe7h
    3ac68cc4e7b8        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         39 seconds ago      Up 35 seconds                           myservice.5.zx4ezdrm2nwxzkrwnxthv0284
    006c3cb318fc        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         39 seconds ago      Up 36 seconds                           myservice.8.q0e3umt19y3h3gzo1ty336k5r
    dd2ffebde435        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         39 seconds ago      Up 36 seconds                           myservice.7.a77y3u22prjipnrjg7vzpv3ba
    a86c74d8b84b        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         39 seconds ago      Up 36 seconds                           myservice.6.z9nbn14bagitwol1biveeygl7
    2846a7850ba0        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         39 seconds ago      Up 37 seconds                           myservice.2.ypufz2eh9fyhppgb89g8wtj76
    e2ec01efcd8a        mrjana/simpleweb@sha256:317d7f221d68c86d503119b0ea12c29de42af0a22ca087d522646ad1069a47a4   "simpleweb"         39 seconds ago      Up 38 seconds                           myservice.1.8w7c4ttzr6zcb9sjsqyhwp3yl
    ```

    On node 2:

    ```bash
    $ docker ps
    CONTAINER ID        IMAGE               COMMAND             CREATED             STATUS              PORTS               NAMES
    ```
