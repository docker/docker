<!--[metadata]>
+++
title = "Access authorization plugin"
description = "How to create authorization plugins to manage access control to your Docker daemon."
keywords = ["security, authorization, authentication, docker, documentation, plugin, extend"]
[menu.main]
parent = "mn_extend"
weight = -1
+++
<![end-metadata]-->


# Access authorization

By default Docker’s authorization model is all or nothing. Anyone who has access to the Docker daemon has the ability to run any Docker command. Authorization plugins enable creating granular access policies for managing access to Docker daemon. 

## Basic principles

* The authorization sub-system is based on Docker's [Plugin API](http://docs.docker.com/engine/extend/plugin_api), so that anyone could add / develop an authorization plug-in. It is possible to add a plug-in to a deployed Docker daemon without the need to rebuild the daemon. Daemon restart is required.

* The authorization sub-system enables approving or denying requests to the Docker daemon based on the current user and command context, where the authentication context contains all user details and the authentication method, and the command context contains all the relevant request data.

* The authorization sub-system enables approving or denying the response from the Docker daemon based on the current user and the command context.

## Basic architecture

Authorization is integrated into Docker daemon by enabling external authorization plug-ins registration as part of the daemon stratup. Each plug-in will receive user's information (retrieved using the authentication sub-system) and HTTP request information and decide whether to allow or deny the request. Plugins can be chained together and only in case where all plug-ins allow accessing the resource, the is access granted. 

Recently Docker introduced a new extendability mechanism called [plug-in infrastructure](http://docs.docker.com/engine/extend/plugin_api), which enables extending Docker by dynamically loading, removing and communicating with third-party components using a generic, easily extendable, API. The authorization sub-system was build using this mechanism. 

To enable full extendability, each plugin receives the authenticated user name, the request context including the user, HTTP headers, and the request/response body. No authentication information will be passed to the authorization plug-in except for: 1) user name, and  2) authentication method that was used. Specifically, no user credentials or tokens will be passed. The request / response bodies will be sent to the plugin only in case the Content-Type is either `text/*` or `application/json`.

For commands that involve the hijacking of the HTTP connection (HTTP Upgrade), such as `exec`, the authorization plugins will only be called for the initial HTTP requests, once the commands are approved, authorization will not be applied to the rest of the flow. Specifically, the streaming data will not be passed to the authorization plugins. For commands that return chunked HTTP response, such as `logs` and `events`, only the HTTP request will be sent to the authorization plug-ins as well. 

During request / response processing, some authorization plugins flows might require performing additional queries to the docker daemon. To complete such a flows plugins can call the daemon API similar to the regular user, which means the plugin admin will have to configure proper authentication and security policies.

## API schema

In addition to the standard plugin registration method, each plugin should implement the following two methods:

`/AuthzPlugin.AuthZReq` The authorize request method, which is called before Docker daemon processes the client request. 

`/AuthzPlugin.AuthZRes` The authorize response method, which is called before the response is returned from Docker daemon to the client. 


### Request authorization 
#### Daemon -> Plugin

Name                   | Type              | Description
-----------------------|-------------------|-------------------------------------------------------
User                   | string            | The user identification
Authentication method  | string            | The authentication method used
Request method         | enum              | The HTTP method (GET/DELETE/POST)
Request URI            | string            | The HTTP request URI including API version (e.g., v.1.17/containers/json)
Request headers        | map[string]string | Request headers as key value pairs (without the authorization header)
Request body           | []byte            | Raw request body


#### Plugin -> Daemon

Name    | Type   | Description
--------|--------|----------------------------------------------------------------------------------
Allow   | bool   | Boolean value indicating whether the request is allowed or denied
Message | string | Authorization message (will be returned to the client in case the access is denied)

### Response authorization 
#### Daemon -> Plugin


Name                    | Type              | Description
----------------------- |------------------ |----------------------------------------------------
User                    | string            | The user identification
Authentication method   | string            | The authentication method used
Request method          | string            | The HTTP method (GET/DELETE/POST)
Request URI             | string            | The http request URI including API version (e.g., v.1.17/containers/json)
Request headers         | map[string]string | Request headers as key value pairs (without the authorization header)
Request body            | []byte            | Raw request body
Response status code    | int               | Status code from the docker daemon
Response headers        | map[string]string | Response headers as key value pairs
Response body           | []byte            | Raw docker daemon response body


#### Plugin -> Daemon

Name    | Type   | Description
--------|--------|----------------------------------------------------------------------------------
Allow   | bool   | Boolean value indicating whether the response is allowed or denied
Message | string | Authorization message (will be returned to the client in case the access is denied)

## UX flows

### Setting up docker daemon 

Authorization plugins are enabled with a dedicated command line argument. The argument contains the plugin name, which should be the same as the plugin’s socket or spec file.
Multiple authz-plugin parameters are supported.
```
$ docker daemon --authz-plugins=plugin1 --auth-plugins=plugin2,...
```

### Calling authorized command (allow)

No impact on command output
```
$ docker pull centos
...
f1b10cd84249: Pull complete 
...
```

### Calling unauthorized command (deny)

```
$ docker pull centos
…
Authorization failed. Pull command for user 'john_doe' is denied by authorization plugin 'ACME' with message ‘[ACME] User 'john_doe' is not allowed to perform the pull command’
```




