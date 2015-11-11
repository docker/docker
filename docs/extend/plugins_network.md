<!--[metadata]>
+++
title = "Docker network driver plugins"
description = "Network driver plugins."
keywords = ["Examples, Usage, plugins, docker, documentation, user guide"]
[menu.main]
parent = "mn_extend"
+++
<![end-metadata]-->

# Docker network driver plugins

Docker network plugins enable Docker deployments to be extended to support a
wide range of networking technologies, such as VXLAN, IPVLAN, MACVLAN or
something completely different. Network driver plugins are supported via the
LibNetwork project. Each plugin is implemented asa  "remote driver" for
LibNetwork, which shares plugin infrastructure with Docker. Effectively,
network driver plugins are activated in the same way as other plugins, and use
the same kind of protocol.

## Using network driver plugins

The means of installing and running a network driver plugin depend on the
particular plugin. So, be sure to install your plugin according to the
instructions obtained from the plugin developer.

Once running however, network driver plugins are used just like the built-in
network drivers: by being mentioned as a driver in network-oriented Docker
commands. For example,

    $ docker network create --driver weave mynet

Some network driver plugins are listed in [plugins](plugins.md)

The `mynet` network is now owned by `weave`, so subsequent commands
referring to that network will be sent to the plugin,

    $ docker run --net=mynet busybox top


## Write a network plugin

Network plugins implement the [Docker plugin
API](https://docs.docker.com/extend/plugin_api/) and the network plugin protocol

## Network plugin protocol

The network driver protocol, in addition to the plugin activation call, is
documented as part of libnetwork:
[https://github.com/docker/libnetwork/blob/master/docs/remote.md](https://github.com/docker/libnetwork/blob/master/docs/remote.md).

# Related Information

To interact with the Docker maintainers and other interested users, se the IRC channel `#docker-network`.

-  [Docker networks feature overview](../userguide/networking/index.md)
-  The [LibNetwork](https://github.com/docker/libnetwork) project
