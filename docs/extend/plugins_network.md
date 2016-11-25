---
title: "Docker network driver plugins"
description: "Network driver plugins."
keywords: "Examples, Usage, plugins, docker, documentation, user guide"
---

<!-- This file is maintained within the docker/docker Github
     repository at https://github.com/docker/docker/. Make all
     pull requests against that repo. If you see this file in
     another repository, consider it read-only there, as it will
     periodically be overwritten by the definitive file. Pull
     requests which include edits to this file in other repositories
     will be rejected.
-->

# Engine network driver plugins

This document describes Docker Engine network driver plugins generally
available in Docker Engine. To view information on plugins
managed by Docker Engine, refer to [Docker Engine plugin system](index.md).

Docker Engine network plugins enable Engine deployments to be extended to
support a wide range of networking technologies, such as VXLAN, IPVLAN, MACVLAN
or something completely different. Network driver plugins are supported via the
LibNetwork project. Each plugin is implemented as a  "remote driver" for
LibNetwork, which shares plugin infrastructure with Engine. Effectively, network
driver plugins are activated in the same way as other plugins, and use the same
kind of protocol.

## Network driver plugins and swarm mode

Docker 1.12 adds support for cluster management and orchestration called
[swarm mode](https://docs.docker.com/engine/swarm/). Docker Engine running in swarm mode currently
only supports the built-in overlay driver for networking. Therefore existing
networking plugins will not work in swarm mode.

When you run Docker Engine outside of swarm mode, all networking plugins that
worked in Docker 1.11 will continue to function normally. They do not require
any modification.

## Using network driver plugins

The means of installing and running a network driver plugin depend on the
particular plugin. So, be sure to install your plugin according to the
instructions obtained from the plugin developer.

Once running however, network driver plugins are used just like the built-in
network drivers: by being mentioned as a driver in network-oriented Docker
commands. For example,

    $ docker network create --driver weave mynet

Some network driver plugins are listed in [plugins](legacy_plugins.md)

The `mynet` network is now owned by `weave`, so subsequent commands
referring to that network will be sent to the plugin,

    $ docker run --network=mynet busybox top


## Write a network plugin

Network plugins implement the [Docker plugin
API](plugin_api.md) and the network plugin protocol

## Network plugin protocol

The network driver protocol, in addition to the plugin activation call, is
documented as part of libnetwork:
[https://github.com/docker/libnetwork/blob/master/docs/remote.md](https://github.com/docker/libnetwork/blob/master/docs/remote.md).

# Related Information

To interact with the Docker maintainers and other interested users, see the IRC channel `#docker-network`.

-  [Docker networks feature overview](https://docs.docker.com/engine/userguide/networking/)
-  The [LibNetwork](https://github.com/docker/libnetwork) project
