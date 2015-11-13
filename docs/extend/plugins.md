<!--[metadata]>
+++
title = "Extending Docker with plugins"
description = "How to add additional functionality to Docker with plugins extensions"
keywords = ["Examples, Usage, plugins, docker, documentation, user guide"]
[menu.main]
parent = "mn_extend"
weight=-1
+++
<![end-metadata]-->

# Understand Docker plugins

You can extend the capabilities of the Docker Engine by loading third-party
plugins. This page explains the types of plugins and provides links to several
volume and network plugins for Docker.

## Types of plugins

Plugins extend Docker's functionality.  They come in specific types.  For
example, a [volume plugin](plugins_volume.md) might enable Docker
volumes to persist across multiple Docker hosts and a
[network plugin](plugins_network.md) might provide network plumbing.

Currently Docker supports volume and network driver plugins. In the future it
will support additional plugin types.

## Installing a plugin

Follow the instructions in the plugin's documentation.

## Finding a plugin

The following plugins exist:

* The [Blockbridge plugin](https://github.com/blockbridge/blockbridge-docker-volume)
  is a volume plugin that provides access to an extensible set of
  container-based persistent storage options. It supports single and multi-host Docker
  environments with features that include tenant isolation, automated
  provisioning, encryption, secure deletion, snapshots and QoS.

* The [Convoy plugin](https://github.com/rancher/convoy) is a volume plugin for a
  variety of storage back-ends including device mapper and NFS. It's a simple standalone
  executable written in Go and provides the framework to support vendor-specific extensions
  such as snapshots, backups and restore.

* The [Flocker plugin](https://clusterhq.com/docker-plugin/) is a volume plugin
  which provides multi-host portable volumes for Docker, enabling you to run
  databases and other stateful containers and move them around across a cluster
  of machines.

* The [GlusterFS plugin](https://github.com/calavera/docker-volume-glusterfs) is
  another volume plugin that provides multi-host volumes management for Docker
  using GlusterFS.

* The [Keywhiz plugin](https://github.com/calavera/docker-volume-keywhiz) is
  a plugin that provides credentials and secret management using Keywhiz as
  a central repository.

* The [Netshare plugin](https://github.com/gondor/docker-volume-netshare) is a volume plugin
  that provides volume management for NFS 3/4, AWS EFS and CIFS file systems.

* The [OpenStorage Plugin](https://github.com/libopenstorage/openstorage) is a cluster aware volume plugin that provides volume management for file and block storage solutions.  It implements a vendor neutral specification for implementing extensions such as CoS, encryption, and snapshots.   It has example drivers based on FUSE, NFS, NBD and EBS to name a few.

* The [Pachyderm PFS plugin](https://github.com/pachyderm/pachyderm/tree/master/src/cmd/pfs-volume-driver)
  is a volume plugin written in Go that provides functionality to mount Pachyderm File System (PFS)
  repositories at specific commits as volumes within Docker containers.

* The [REX-Ray plugin](https://github.com/emccode/rexraycli) is a volume plugin
  which is written in Go and provides advanced storage functionality for many
  platforms including EC2, OpenStack, XtremIO, and ScaleIO.

* The [Contiv Volume Plugin](https://github.com/contiv/volplugin) is an open
source volume plugin that provides multi-tenant, persistent, distributed storage
with intent based consumption using ceph underneath.

* The [Contiv Networking](https://github.com/contiv/netplugin) is an open source
libnetwork plugin to provide infrastructure and security policies for a
multi-tenant micro services deployment, while providing an integration to
physical network for non-container workload. Contiv Networking implements the
remote driver and IPAM APIs available in Docker 1.9 onwards.

* The [Weave Network Plugin](https://github.com/weaveworks/docker-plugin) creates a virtual network that connects your Docker containers - across multiple hosts or clouds and enables automatic discovery of applications. Weave networks are resilient, partition tolerant, secure and work in partially connected networks, and other adverse environments - all configured with delightful simplicity.

## Troubleshooting a plugin

If you are having problems with Docker after loading a plugin, ask the authors
of the plugin for help. The Docker team may not be able to assist you.

## Writing a plugin

If you are interested in writing a plugin for Docker, or seeing how they work
under the hood, see the [docker plugins reference](plugin_api.md).
