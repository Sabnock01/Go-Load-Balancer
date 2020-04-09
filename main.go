package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	URL				*url.URL
	Alive			bool
	mux				sync.RWMutex
	ReverseProxy	*httputil.ReverseProxy
}

type ServerPool struct {
	backends		[]*Backend
	current			uint64
}

var sP ServerPool

const (
	Attempts int = iota
	Retry
)

func (s *ServerPool) AddBackend(backend *Backend) {
	s.backends = append(s.backends, backend)
}

func (s *ServerPool) BackendStatus(backendUrl *url.URL, alive bool) {
	for _, b := range s.backends {
		if b.URL.String() == backendUrl.String() {
			b.SetAlive(alive)
			break
		}
	}
}

func (s *ServerPool) NextIndex() int {
	return int(atomic.AddUint64(&s.current, uint64(1)) % uint64(len(s.backends)))
}

func (s *ServerPool) NextPeer() *Backend {
	next := s.NextIndex()
	l := len(s.backends) + next
	for i := next; i < l; i++ {
		idx := i % len(s.backends)
		if s.backends[idx].GetAlive() {
			if i != next {
				atomic.StoreUint64(&s.current, uint64(idx))
			}
			return s.backends[idx]
		}
	}
	return nil
}

func (b *Backend) GetAlive() (alive bool) {
	b.mux.RLock()
	alive = b.Alive
	b.mux.RUnlock()
	return
}

func (b *Backend) SetAlive(alive bool) {
	b.mux.Lock()
	b.Alive = alive
	b.mux.Unlock()
}

func (s *ServerPool) HealthCheck() {
	for _, b := range s.backends {
	  status := "up"
	  alive := isBackendAlive(b.URL)
	  b.SetAlive(alive)
	  if !alive {
		status = "down"
	  }
	  log.Printf("%s [%s]\n", b.URL, status)
	}
}

func GetAttempts(r *http.Request) int {
	if attempts, ok := r.Context().Value(Attempts).(int); ok {
		return attempts
	}
	return 1
}

func GetRetry(r *http.Request) int {
	if retry, ok := r.Context().Value(Retry).(int); ok {
	  return retry
	}
	return 00
}

func isBackendAlive(u *url.URL) bool {
	timeout := 2 * time.Second
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
	  log.Println("Site unreachable, error: ", err)
	  return false
	}
	_ = conn.Close()
	return true
}

func healthCheck() {
	t := time.NewTicker(time.Second * 20)
	for {
	  select {
	  case <-t.C:
		log.Println("Starting health check...")
		sP.HealthCheck()
		log.Println("Health check completed")
	  }
	}
}

func lb(w http.ResponseWriter, r *http.Request) {
	attempts := GetAttempts(r)
	if attempts > 3 {
		log.Printf("%s(%s) Max attempts reached\n", r.RemoteAddr, r.URL.Path)
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}
	peer := sP.NextPeer()
	if peer != nil {
	  peer.ReverseProxy.ServeHTTP(w, r)
	  return
	}
	http.Error(w, "Service not available", http.StatusServiceUnavailable)
}

func main() {
	var serverList string
	var port int
	flag.StringVar(&serverList, "backends", "", "Load balanced backends, use commas to separate")
	flag.IntVar(&port, "port", 3030, "Port to serve")
	flag.Parse()

	if len(serverList) == 0 {
		log.Fatal("No backends have been provided.")
	}

	tokens := strings.Split(serverList, ",")
	for _, tok := range tokens {
		serverUrl, err := url.Parse(tok)
		if err != nil {
			log.Fatal(err)
		}

		proxy := httputil.NewSingleHostReverseProxy(serverUrl)
		proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, e error) {
			log.Printf("[%s] %s\n", serverUrl.Host, e.Error())
			retries := GetRetry(request)
			if retries < 3 {
				select {
				case <-time.After(10 * time.Millisecond):
					ctx := context.WithValue(request.Context(), Retry, retries+1)
					proxy.ServeHTTP(writer, request.WithContext(ctx))
				}
				return
			}

			sP.BackendStatus(serverUrl, false)

			attempts := GetAttempts(request)
			log.Printf("%s(%s) Attempting retry %d\n", request.RemoteAddr, request.URL.Path, attempts)
			ctx := context.WithValue(request.Context(), Attempts, attempts+1)
			lb(writer, request.WithContext(ctx))
		}

		sP.AddBackend(&Backend{
			URL:          serverUrl,
			Alive:        true,
			ReverseProxy: proxy,
		})
		log.Printf("Configured server: %s\n", serverUrl)
	}

	server := http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: http.HandlerFunc(lb),
	}

	go healthCheck()

	log.Printf("Load Balancer started at :%d\n", port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
