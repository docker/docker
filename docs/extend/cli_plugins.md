---
description: "Writing Docker CLI Plugins"
keywords: "docker, cli plugin"
---

<!-- This file is maintained within the docker/cli GitHub
     repository at https://github.com/docker/cli/. Make all
     pull requests against that repo. If you see this file in
     another repository, consider it read-only there, as it will
     periodically be overwritten by the definitive file. Pull
     requests which include edits to this file in other repositories
     will be rejected.
-->

# Docker CLI Plugin Spec

The `docker` CLI supports adding additional top-level subcommands as
additional out-of-process commands which can be installed
independently. These plugins run on the client side and should not be
confused with "plugins" which run on the server.

This document contains information for authors of such plugins.

## Requirements for CLI Plugins

### Naming

A valid CLI plugin name consists only of lower case letters `a-z`
and the digits `0-9`. The leading character must be a letter. A valid
name therefore would match the regex `^[a-z][a-z0-9]*$`.

The binary implementing a plugin must be named `docker-$name` where
`$name` is the name of the plugin. On Windows a `.exe` suffix is
mandatory.

## Required sub-commands

A CLI plugin must support being invoked in at least these two ways:

* `docker-$name docker-cli-plugin-metadata` -- outputs metadata about
  the plugin.
* `docker-$name [GLOBAL OPTIONS] $name [OPTIONS AND FURTHER SUB
  COMMANDS]` -- the primary entry point to the plugin's functionality.

A plugin may implement other subcommands but these will never be
invoked by the current Docker CLI. However doing so is strongly
discouraged: new subcommands may be added in the future without
consideration for additional non-specified subcommands which may be
used by plugins in the field.

### The `docker-cli-plugin-metadata` subcommand

When invoked in this manner the plugin must produce a JSON object
(and nothing else) on its standard output and exit success (0).

The JSON object has the following defined keys:
* `SchemaVersion` (_string_) mandatory: must contain precisely "0.1.0".
* `Vendor` (_string_) mandatory: contains the name of the plugin vendor/author. May be truncated to 11 characters in some display contexts.
* `ShortDescription` (_string_) optional: a short description of the plugin, suitable for a single line help message.
* `Version` (_string_) optional: the version of the plugin, this is considered to be an opaque string by the core and therefore has no restrictions on its syntax.
* `URL` (_string_) optional: a pointer to the plugin's web page.

A binary which does not correctly output the metadata
(e.g. syntactically invalid, missing mandatory keys etc) is not
considered a valid CLI plugin and will not be run.

### The primary entry point subcommand

This is the entry point for actually running the plugin. It maybe have
options or further subcommands.

#### Required global options

A plugin is required to support all of the global options of the
top-level CLI, i.e. those listed by `man docker 1` with the exception
of `-v`.

## Installation

Plugins distributed in packages for system wide installation on
Unix(-like) systems should be installed in either
`/usr/lib/docker/cli-plugins` or `/usr/libexec/docker/cli-plugins`
depending on which of `/usr/lib` and `/usr/libexec` is usual on that
system. System Administrators may also choose to manually install into
the `/usr/local/lib` or `/usr/local/libexec` equivalents but packages
should not do so.

Plugins distributed on Windows for system wide installation should be
installed in `%PROGRAMDATA%\Docker\cli-plugins`.

User's may on all systems install plugins into `~/.docker/cli-plugins`.

## Implementing a plugin in Go

When writing a plugin in Go the easiest way to meet the above
requirements is to simply call the
`github.com/docker/cli/cli-plugins/plugin.Run` method from your `main`
function to instantiate the plugin.
