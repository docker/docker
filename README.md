The Moby Project
================

![Moby Project logo](docs/static_files/moby-project-logo.png "The Moby Project")

Moby is an open-source project created by Docker to enable and accelerate software containerization.

It provides a "Lego set" of toolkit components, the framework for assembling them into custom container-based systems, and a place for all container enthusiasts and professionals to experiment and exchange ideas.
Components include container build tools, a container registry, orchestration tools, a runtime, and more, and these can be used as building blocks in conjunction with other tools and projects.

## Principles

Moby is an open project guided by strong principles, aiming to be modular, flexible, and without too strong an opinion on user experience.
It is open to the community to help set its direction.

- Modular: the project includes lots of components that have well-defined functions and APIs that work together.

- Batteries included but swappable: Moby includes enough components to build a fully-featured container system, but its modular architecture ensures that most of the components can be swapped by different implementations.

- Usable security: Moby provides secure defaults without compromising usability.

- Developer-focused: The APIs are intended to be functional and useful to build powerful tools.

  They are not necessarily intended as end-user tools but as components aimed at developers.
  Documentation and UX are aimed at developers not end-users.

## Audience

The Moby Project is intended for engineers, integrators, and enthusiasts looking to modify, hack, fix, experiment, invent and build systems based on containers.
It is not for people looking for a commercially supported system, but for people who want to work and learn with open source code.

## Relationship with Docker

The components and tools in the Moby Project are initially the open source components that Docker and the community have built for the Docker Project.
New projects can be added if they fit with the community goals. Docker is committed to using Moby as the upstream for the Docker Product.
However, other projects are also encouraged to use Moby as an upstream and to reuse the components in diverse ways, and all these uses will be treated in the same way. External maintainers and contributors are welcomed.

The Moby project is not intended as a location for support or feature requests for Docker products, but as a place for contributors to work on open source code, fix bugs, and make the code more useful.
The releases are supported by the maintainers, community, and users, on a best efforts basis only, and are not intended for customers who want an enterprise or commercial support; Docker EE is the appropriate product for these use cases.

-----

Legal
=====

*Brought to you courtesy of our legal counsel. For more context,
please see the [NOTICE](https://github.com/moby/moby/blob/master/NOTICE) document in this repo.*

Use and transfer of Moby may be subject to certain restrictions by the
United States and other governments.

It is your responsibility to ensure that your use and/or transfer does not
violate applicable laws.

For more information, please see https://www.bis.doc.gov

Licensing
=========
Moby is licensed under the Apache License, Version 2.0. See
[LICENSE](https://github.com/moby/moby/blob/master/LICENSE) for the full
license text.