// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package go126flate contains the DEFLATE writer from Go 1.26.5.
//
// The sources are copied from Go commit
// c19862e5f8415b4f24b189d065ed739517c548ba. Go 1.27 changed the encoded
// output while improving compression speed in CL 707355 for golang/go#75532.
// BuildKit keeps this writer to preserve the digests of gzip-compressed layers.
package go126flate

// maxNumLit comes from RFC 1951 section 3.2.7. It normally lives in the Go
// flate decoder, which is intentionally not included in this writer-only copy.
const maxNumLit = 286

// InternalError reports an error in the flate code itself.
type InternalError string

func (e InternalError) Error() string { return "flate: internal error: " + string(e) }
