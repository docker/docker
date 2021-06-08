## containerd Adopters

A non-exhaustive list of containerd adopters is provided below.

**_Docker/Moby engine_** - Containerd began life prior to its CNCF adoption as a lower-layer
runtime manager for `runc` processes below the Docker engine. Continuing today, containerd
has extremely broad production usage as a component of the [Docker engine](https://github.com/docker/docker-ce)
stack. Note that this includes any use of the open source [Moby engine project](https://github.com/moby/moby);
including the Balena project listed below.

**_[IBM Cloud Kubernetes Service (IKS)](https://www.ibm.com/cloud/container-service)_** - offers containerd as the CRI runtime for v1.11 and higher versions.

**_[IBM Cloud Private (ICP)](https://www.ibm.com/cloud/private)_** - IBM's on-premises cloud offering has containerd as a "tech preview" CRI runtime for the Kubernetes offered within this product for the past two releases, and plans to fully migrate to containerd in a future release.

**_[Google Cloud Kubernetes Engine (GKE)](https://cloud.google.com/kubernetes-engine/)_** - offers containerd as the CRI runtime in **beta** for recent versions of Kubernetes.

**_[AWS Fargate](https://aws.amazon.com/fargate)_** - uses containerd + Firecracker (noted below) as the runtime and isolation technology for containers run in the Fargate platform. Fargate is a serverless, container-native compute offering from Amazon Web Services.

**_Cloud Foundry_** - The [Guardian container manager](https://github.com/cloudfoundry/guardian) for CF has been using OCI runC directly with additional code from CF managing the container image and filesystem interactions, but have recently migrated to use containerd as a replacement for the extra code they had written around runC.

**_Alibaba's PouchContainer_** - The Alibaba [PouchContainer](https://github.com/alibaba/pouch) project uses containerd as its runtime for a cloud native offering that has unique isolation and image distribution capabilities.

**_Rancher's k3s project_** - Rancher Labs [k3s](https://github.com/rancher/k3s) is a lightweight Kubernetes distribution; in their words: "Easy to install, half the memory, all in a binary less than 40mb." k8s uses containerd as the embedded runtime for this popular lightweight Kubernetes variant.

**_Rancher's Rio project_** - Rancher Labs [Rio](https://github.com/rancher/rio) project uses containerd as the runtime for a combined Kubernetes, Istio, and container "Cloud Native Container Distribution" platform.

**_Eliot_** - The [Eliot](https://github.com/ernoaapa/eliot) container project for IoT device container management uses containerd as the runtime.

**_Balena_** - Resin's [Balena](https://github.com/resin-os/balena) container engine, based on moby/moby but for edge, embedded, and IoT use cases, uses the containerd and runc stack in the same way that the Docker engine uses containerd.

**_LinuxKit_** - the Moby project's [LinuxKit](https://github.com/linuxkit/linuxkit) for building secure, minimal Linux OS images in a container-native model uses containerd as the core runtime for system and service containers.

**_BuildKit_** - The Moby project's [BuildKit](https://github.com/moby/buildkit) can use either runC or containerd as build execution backends for building container images. BuildKit support has also been built into the Docker engine in recent releases, making BuildKit provide the backend to the `docker build` command.

**_Azure acs-engine_** - Microsoft Azure's [acs-engine](https://github.com/Azure/acs-engine) open source project has customizable deployment of Kubernetes clusters, where containerd is a selectable container runtime. At some point in the future Azure's AKS service will default to use containerd as the CRI runtime for deployed Kubernetes clusters.

**_Amazon Firecracker_** - The AWS [Firecracker VMM project](http://firecracker-microvm.io/) has extended containerd with a new snapshotter and v2 shim to allow containerd to drive virtualized container processes via their VMM implementation. More details on their containerd integration are available in [their GitHub project](https://github.com/firecracker-microvm/firecracker-containerd).

**_Kata Containers_** - The [Kata containers](https://katacontainers.io/) lightweight-virtualized container runtime project integrates with containerd via a custom v2 shim implementation that drives the Kata container runtime.

**_D2iQ Konvoy_** - D2iQ Inc [Konvoy](https://d2iq.com/products/konvoy) product uses containerd as the container runtime for its Kubernetes distribution.

**_Inclavare Containers_** - [Inclavare Containers](https://github.com/alibaba/inclavare-containers) is an innovation of container runtime with the novel approach for launching protected containers in hardware-assisted Trusted Execution Environment (TEE) technology, aka Enclave, which can prevent the untrusted entity, such as Cloud Service Provider (CSP), from accessing the sensitive and confidential assets in use.

**_Other Projects_** - While the above list provides a cross-section of well known uses of containerd, the simplicity and clear API layer for containerd has inspired many smaller projects around providing simple container management platforms. Several examples of building higher layer functionality on top of the containerd base have come from various containerd community participants:
 - Michael Crosby's [boss](https://github.com/crosbymichael/boss) project,
 - Evan Hazlett's [stellar](https://github.com/ehazlett/stellar) project,
 - Paul Knopf's immutable Linux image builder project: [darch](https://github.com/godarch/darch).
