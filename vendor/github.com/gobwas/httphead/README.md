# httphead.[go](https://golang.org)

[![GoDoc][godoc-image]][godoc-url] 

> Tiny HTTP header value parsing library in go.

## Overview

This library contains low-level functions for scanning HTTP RFC2616 compatible header value grammars.

## Install

```shell
    go get github.com/gobwas/httphead
```

## Example

The example below shows how multiple-choise HTTP header value could be parsed with this library:

```go
	options, ok := httphead.ParseOptions([]byte(`foo;bar=1,baz`), nil)
	fmt.Println(options, ok)
	// Output: [{foo map[bar:1]} {baz map[]}] true
```

The low-level example below shows how to optimize keys skipping and selection
of some key:

```go
	// The right part of full header line like:
	// X-My-Header: key;foo=bar;baz,key;baz
	header := []byte(`foo;a=0,foo;a=1,foo;a=2,foo;a=3`)

	// We want to search key "foo" with an "a" parameter that equal to "2".
	var (
		foo = []byte(`foo`)
		a   = []byte(`a`)
		v   = []byte(`2`)
	)
	var found bool
	httphead.ScanOptions(header, func(i int, key, param, value []byte) Control {
		if !bytes.Equal(key, foo) {
			return ControlSkip
		}
		if !bytes.Equal(param, a) {
			if bytes.Equal(value, v) {
				// Found it!
				found = true
				return ControlBreak
			}
			return ControlSkip
		}
		return ControlContinue
	})
```

For more usage examples please see [docs][godoc-url] or package tests.

[godoc-image]: https://godoc.org/github.com/gobwas/httphead?status.svg
[godoc-url]: https://godoc.org/github.com/gobwas/httphead
[travis-image]: https://travis-ci.org/gobwas/httphead.svg?branch=master
[travis-url]: https://travis-ci.org/gobwas/httphead
