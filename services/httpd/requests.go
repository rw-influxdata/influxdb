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

type RequestTracker struct {
	requests map[RequestInfo]*int64
	mu       sync.Mutex
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

func (rt *RequestTracker) Close() error {
	close(rt.done)
	rt.wg.Wait()
	return nil
}

func (rt *RequestTracker) loop() {
	defer rt.wg.Done()

	var wg sync.WaitGroup
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-rt.done:
			wg.Wait()
			return
		}

		rt.mu.Lock()
		if len(rt.requests) == 0 {
			continue
		}

		requests := rt.requests
		rt.requests = make(map[RequestInfo]*int64, len(requests))
		rt.mu.Unlock()

		wg.Add(1)
		go func(requests map[RequestInfo]*int64) {
			defer wg.Done()
			rt.report(requests)
		}(requests)
	}
}

func (rt *RequestTracker) report(requests map[RequestInfo]*int64) {
	fmt.Printf("There have been requests from %d unique locations\n", len(requests))
}
