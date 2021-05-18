// +build pool_sanitize

package pbytes

import (
	"reflect"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const magic = uint64(0x777742)

type guard struct {
	magic  uint64
	size   int
	owners int32
}

const guardSize = int(unsafe.Sizeof(guard{}))

type Pool struct {
	min, max int
}

func New(min, max int) *Pool {
	return &Pool{min, max}
}

// Get returns probably reused slice of bytes with at least capacity of c and
// exactly len of n.
func (p *Pool) Get(n, c int) []byte {
	if n > c {
		panic("requested length is greater than capacity")
	}

	pageSize := syscall.Getpagesize()
	pages := (c+guardSize)/pageSize + 1
	size := pages * pageSize

	bts := alloc(size)

	g := (*guard)(unsafe.Pointer(&bts[0]))
	*g = guard{
		magic:  magic,
		size:   size,
		owners: 1,
	}

	return bts[guardSize : guardSize+n]
}

func (p *Pool) GetCap(c int) []byte { return p.Get(0, c) }
func (p *Pool) GetLen(n int) []byte { return Get(n, n) }

// Put returns given slice to reuse pool.
func (p *Pool) Put(bts []byte) {
	hdr := *(*reflect.SliceHeader)(unsafe.Pointer(&bts))
	ptr := hdr.Data - uintptr(guardSize)

	g := (*guard)(unsafe.Pointer(ptr))
	if g.magic != magic {
		panic("unknown slice returned to the pool")
	}
	if n := atomic.AddInt32(&g.owners, -1); n < 0 {
		panic("multiple Put() detected")
	}

	// Disable read and write on bytes memory pages. This will cause panic on
	// incorrect access to returned slice.
	mprotect(ptr, false, false, g.size)

	runtime.SetFinalizer(&bts, func(b *[]byte) {
		mprotect(ptr, true, true, g.size)
		free(*(*[]byte)(unsafe.Pointer(&reflect.SliceHeader{
			Data: ptr,
			Len:  g.size,
			Cap:  g.size,
		})))
	})
}

func alloc(n int) []byte {
	b, err := unix.Mmap(-1, 0, n, unix.PROT_READ|unix.PROT_WRITE|unix.PROT_EXEC, unix.MAP_SHARED|unix.MAP_ANONYMOUS)
	if err != nil {
		panic(err.Error())
	}
	return b
}

func free(b []byte) {
	if err := unix.Munmap(b); err != nil {
		panic(err.Error())
	}
}

func mprotect(ptr uintptr, r, w bool, size int) {
	// Need to avoid "EINVAL addr is not a valid pointer,
	// or not a multiple of PAGESIZE."
	start := ptr & ^(uintptr(syscall.Getpagesize() - 1))

	prot := uintptr(syscall.PROT_EXEC)
	switch {
	case r && w:
		prot |= syscall.PROT_READ | syscall.PROT_WRITE
	case r:
		prot |= syscall.PROT_READ
	case w:
		prot |= syscall.PROT_WRITE
	}

	_, _, err := syscall.Syscall(syscall.SYS_MPROTECT,
		start, uintptr(size), prot,
	)
	if err != 0 {
		panic(err.Error())
	}
}
