package httpd

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/influxdata/influxdb/services/meta"
)

type RequestInfo struct {
	IPAddr   string
	Username string
}

type RequestTracker struct {
	requests map[RequestInfo]int
	mu       sync.Mutex
	wg       sync.WaitGroup
	done     chan struct{}
}

func NewRequestTracker() *RequestTracker {
	rt := &RequestTracker{
		requests: make(map[RequestInfo]int),
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

	rt.mu.Lock()
	rt.requests[info]++
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
			rt.mu.Unlock()
			continue
		}

		requests := rt.requests
		rt.requests = make(map[RequestInfo]int, len(requests))
		rt.mu.Unlock()

		wg.Add(1)
		go func(requests map[RequestInfo]int) {
			defer wg.Done()
			rt.report(requests)
		}(requests)
	}
}

func (rt *RequestTracker) report(requests map[RequestInfo]int) {
	fmt.Printf("There have been requests from %d unique locations\n", len(requests))
}
