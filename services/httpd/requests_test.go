package httpd_test

import (
	"encoding/binary"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/influxdata/influxdb/services/httpd"
)

func BenchmarkRequestTracker_1K(b *testing.B)   { benchmarkRequestTracker(b, 1000) }
func BenchmarkRequestTracker_100K(b *testing.B) { benchmarkRequestTracker(b, 100000) }
func BenchmarkRequestTracker_1M(b *testing.B)   { benchmarkRequestTracker(b, 1000000) }

func benchmarkRequestTracker(b *testing.B, count int) {
	b.ReportAllocs()

	tracker := httpd.NewRequestTracker()
	defer tracker.Close()

	var start sync.WaitGroup
	start.Add(count)

	var done sync.WaitGroup
	done.Add(count)

	for i := 0; i < count; i++ {
		go func(i int) {
			var buf [4]byte
			binary.BigEndian.PutUint32(buf[:], uint32(i))

			ip := net.IPv4(buf[0], buf[1], buf[2], buf[3])
			req := &http.Request{
				RemoteAddr: ip.String(),
			}
			start.Done()
			start.Wait()

			for i := 0; i < b.N; i++ {
				tracker.Increment(req, nil)
			}
			done.Done()
		}(i)
	}
	done.Wait()
}
