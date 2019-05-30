fluent-logger-golang
====

[![Build Status](https://travis-ci.org/fluent/fluent-logger-golang.png?branch=master)](https://travis-ci.org/fluent/fluent-logger-golang)

## A structured event logger for Fluentd (Golang)

## How to install

```
go get github.com/fluent/fluent-logger-golang/fluent
```

## Usage

Install the package with `go get` and use `import` to include it in your project.

```
import "github.com/fluent/fluent-logger-golang/fluent"
```

GoDoc: http://godoc.org/github.com/fluent/fluent-logger-golang/fluent

## Example

```go
package main

import (
  "github.com/fluent/fluent-logger-golang/fluent"
  "fmt"
  "time"
)

func main() {
  logger, err := fluent.New(fluent.Config{})
  if err != nil {
    fmt.Println(err)
  }
  defer logger.Close()
  tag := "myapp.access"
  var data = map[string]string{
    "foo":  "bar",
    "hoge": "hoge",
  }
  error := logger.Post(tag, data)
  // error := logger.PostWithTime(tag, time.Now(), data)
  if error != nil {
    panic(error)
  }
}
```

`data` must be a value like `map[string]literal`, `map[string]interface{}`, `struct` or [`msgp.Marshaler`](http://godoc.org/github.com/tinylib/msgp/msgp#Marshaler). Logger refers tags `msg` or `codec` of each fields of structs.

## Setting config values

```go
f := fluent.New(fluent.Config{FluentPort: 80, FluentHost: "example.com"})
```

### WriteTimeout

Sets the timeout for Write call of logger.Post.
Since the default is zero value, Write will not time out.

### Async

Enable asynchronous I/O (connect and write) for sending events to Fluentd.
The default is false.

### RequestAck

Sets whether to request acknowledgment from Fluentd to increase the reliability
of the connection. The default is false.

## Tests
```
go test
```
