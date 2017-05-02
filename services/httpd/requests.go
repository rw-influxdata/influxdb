package httpd

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/influxdata/influxdb/services/meta"
)

type RequestInfo struct {
	IPAddr   string
	Username string
}

func (r *RequestInfo) String() string {
	if r.Username != "" {
		return fmt.Sprintf("%s:%s", r.Username, r.IPAddr)
	}
	return r.IPAddr
}

type RequestTracker struct {
	requests map[RequestInfo]*int64
	mu       sync.RWMutex
	wg       sync.WaitGroup
	done     chan struct{}
}

func NewRequestTracker() *RequestTracker {
	rt := &RequestTracker{
		requests: make(map[RequestInfo]*int64),
		done:     make(chan struct{}),
	}
	rt.wg.Add(1)
	go rt.loop()
	return rt
}

func (rt *RequestTracker) Increment(req *http.Request, user *meta.UserInfo) {
	var info RequestInfo
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return
	}

	info.IPAddr = host
	if user != nil {
		info.Username = user.Name
	}

	// There is an entry in the request tracker. Increment that.
	if n, ok := rt.requests[info]; ok {
		atomic.AddInt64(n, 1)
		return
	}

	// There is no entry in the request tracker. Create one.
	rt.mu.Lock()
	if n, ok := rt.requests[info]; ok {
		// Something else created this entry while we were waiting for the lock.
		rt.mu.Unlock()
		atomic.AddInt64(n, 1)
		return
	}

	val := int64(1)
	rt.requests[info] = &val
	rt.mu.Unlock()
}

// Stats returns the current request map from the request tracker. This request
// map is actively written to. Only read from the map (which does not require a
// lock) and use atomic.LoadInt64 to read the count.
func (rt *RequestTracker) Stats() map[RequestInfo]*int64 {
	rt.mu.RLock()
	requests := rt.requests
	rt.mu.RUnlock()
	return requests
}

func (rt *RequestTracker) Close() error {
	close(rt.done)
	rt.wg.Wait()
	return nil
}

func (rt *RequestTracker) loop() {
	defer rt.wg.Done()

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-rt.done:
			return
		}

		rt.mu.Lock()
		if len(rt.requests) == 0 {
			continue
		}

		requests := rt.requests
		rt.requests = make(map[RequestInfo]*int64, len(requests))
		rt.mu.Unlock()
	}
}
