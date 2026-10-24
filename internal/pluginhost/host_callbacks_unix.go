//go:build cgo && (linux || darwin || freebsd)

package pluginhost

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;
*/
import "C"

import (
	"sync"
	"unsafe"
)

// The C context and host API outlive the plugin client. This Go entry is only
// retained while admission is open or a callback is in flight.
type unixHostCallbackEntry struct {
	dynamicHostCallbackEntry
	mu       sync.Mutex
	detached bool
	active   sync.WaitGroup
}

func (e *unixHostCallbackEntry) acquire() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.detached {
		return false
	}
	e.active.Add(1)
	return true
}

func (e *unixHostCallbackEntry) detach() {
	e.mu.Lock()
	e.detached = true
	e.mu.Unlock()
	e.callbacks.detach()
}

//export cliproxyHostCall
func cliproxyHostCall(hostCtx unsafe.Pointer, method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if hostCtx == nil || method == nil {
		return 1
	}
	// Treat userdata as an opaque key: never dereference memory supplied by a
	// late native callback. Its allocation is never freed/reused by the host.
	rawHost, okHost := hostCallbackEntries.Load(hostCtx)
	if !okHost {
		return 1
	}
	entry, okHost := rawHost.(*unixHostCallbackEntry)
	if !okHost || entry.host == nil || !entry.acquire() {
		return 1
	}
	defer entry.active.Done()
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	ctx := entry.context()
	resp, errCall := entry.host.callFromPlugin(ctx, C.GoString(method), requestBytes)
	if errCall != nil {
		resp = marshalRPCError("host_call_failed", errCall.Error())
	}
	if len(resp) == 0 || response == nil {
		return 0
	}
	ptr := C.CBytes(resp)
	if ptr == nil {
		return 1
	}
	response.ptr = ptr
	response.len = C.size_t(len(resp))
	return 0
}

//export cliproxyHostFree
func cliproxyHostFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}
