<!--[metadata]>
+++
title = "Extending Engine with plugins"
description = "How to add additional functionality to Docker with plugins extensions"
keywords = ["Examples, Usage, plugins, docker, documentation, user guide"]
[menu.main]
parent = "engine_extend"
weight=-1
+++
<![end-metadata]-->

# Understand Engine plugins

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

* The [Horcrux Volume Plugin](https://github.com/muthu-r/horcrux) allows on-demand,
  version controlled access to your data. Horcrux is an open-source plugin,
  written in Go, and supports SCP, [Minio](https://www.minio.io) and Amazon S3.

* The [IPFS Volume Plugin](http://github.com/vdemeester/docker-volume-ipfs)
  is an open source volume plugin that allows using an
  [ipfs](https://ipfs.io/) filesystem as a volume.

* The [Keywhiz plugin](https://github.com/calavera/docker-volume-keywhiz) is
  a plugin that provides credentials and secret management using Keywhiz as
  a central repository.

* The [Netshare plugin](https://github.com/gondor/docker-volume-netshare) is a volume plugin
  that provides volume management for NFS 3/4, AWS EFS and CIFS file systems.

* The [gce-docker plugin](https://github.com/mcuadros/gce-docker) is a volume plugin able to attach, format and mount Google Compute [persistent-disks](https://cloud.google.com/compute/docs/disks/persistent-disks).

* The [OpenStorage Plugin](https://github.com/libopenstorage/openstorage) is a cluster aware volume plugin that provides volume management for file and block storage solutions.  It implements a vendor neutral specification for implementing extensions such as CoS, encryption, and snapshots.   It has example drivers based on FUSE, NFS, NBD and EBS to name a few.

* The [Quobyte Volume Plugin](https://github.com/quobyte/docker-volume) connects Docker to [Quobyte](http://www.quobyte.com/containers)'s data center file system, a general-purpose scalable and fault-tolerant storage platform.

* The [REX-Ray plugin](https://github.com/emccode/rexray) is a volume plugin
  which is written in Go and provides advanced storage functionality for many
  platforms including VirtualBox, EC2, Google Compute Engine, OpenStack, and EMC.

* The [Contiv Volume Plugin](https://github.com/contiv/volplugin) is an open
  source volume plugin that provides multi-tenant, persistent, distributed storage
  with intent based consumption using ceph underneath.

* The [Contiv Networking](https://github.com/contiv/netplugin) is an open source
  libnetwork plugin to provide infrastructure and security policies for a
  multi-tenant micro services deployment, while providing an integration to
  physical network for non-container workload. Contiv Networking implements the
  remote driver and IPAM APIs available in Docker 1.9 onwards.

* The [Weave Network Plugin](http://docs.weave.works/weave/latest_release/plugin.html)
  creates a virtual network that connects your Docker containers -
  across multiple hosts or clouds and enables automatic discovery of
  applications. Weave networks are resilient, partition tolerant,
  secure and work in partially connected networks, and other adverse
  environments - all configured with delightful simplicity.

* The [Kuryr Network Plugin](https://github.com/openstack/kuryr) is
  developed as part of the OpenStack Kuryr project and implements the
  Docker networking (libnetwork) remote driver API by utilizing
  Neutron, the OpenStack networking service. It includes an IPAM
  driver as well.

* The [Local Persist Plugin](https://github.com/CWSpear/local-persist) 
  extends the default `local` driver's functionality by allowing you specify 
  a mountpoint anywhere on the host, which enables the files to *always persist*, 
  even if the volume is removed via `docker volume rm`.

## Troubleshooting a plugin

If you are having problems with Docker after loading a plugin, ask the authors
of the plugin for help. The Docker team may not be able to assist you.

## Writing a plugin

If you are interested in writing a plugin for Docker, or seeing how they work
under the hood, see the [docker plugins reference](plugin_api.md).
