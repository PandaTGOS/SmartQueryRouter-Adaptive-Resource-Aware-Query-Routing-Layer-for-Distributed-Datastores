package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

var backendNodes = []string{
	"mongo-proxy1:9001",
	"mongo-proxy2:9001",
	"mongo-proxy3:9001",
}

const (
	healthCheckInterval = 5 * time.Second
	maxRetries          = 3
	prometheusURL       = "http://prometheus:9090"
)

var logger *zap.Logger

// Prometheus metrics
var (
	requestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "smartrouter_requests_total",
			Help: "Total number of requests routed to each backend node.",
		},
		[]string{"node"},
	)
	nodeHealth = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "smartrouter_node_healthy",
			Help: "Health status of a node (1 for healthy, 0 for unhealthy).",
		},
		[]string{"node"},
	)
	queryLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "smartrouter_query_latency_seconds",
			Help:    "Latency of queries routed to each backend node.",
			Buckets: prometheus.LinearBuckets(0.01, 0.01, 10),
		},
		[]string{"node"},
	)
)

type NodeStatus struct {
	IsHealthy bool
	Latency   time.Duration
}

type ScoredNode struct {
	Address string
	Score   float64 // Lower = better
}

type Router struct {
	nodeStatus map[string]*NodeStatus
	mu         sync.RWMutex
}

func NewRouter() *Router {
	r := &Router{
		nodeStatus: make(map[string]*NodeStatus),
	}
	for _, addr := range backendNodes {
		r.nodeStatus[addr] = &NodeStatus{IsHealthy: false}
	}
	go r.startHealthChecker()
	return r
}

func (r *Router) startHealthChecker() {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()
	r.checkAllNodes()
	for range ticker.C {
		r.checkAllNodes()
	}
}

func (r *Router) checkAllNodes() {
	var wg sync.WaitGroup
	for addr := range r.nodeStatus {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
			r.mu.Lock()
			defer r.mu.Unlock()
			if err != nil {
				r.nodeStatus[addr].IsHealthy = false
				nodeHealth.WithLabelValues(addr).Set(0)
			} else {
				r.nodeStatus[addr].IsHealthy = true
				nodeHealth.WithLabelValues(addr).Set(1)
				conn.Close()
			}
		}(addr)
	}
	wg.Wait()
}

func getPrometheusQuantile(node string, quantile float64) float64 {
	query := fmt.Sprintf(`histogram_quantile(%.2f, sum(rate(smartrouter_query_latency_seconds_bucket[1m])) by (le, node))`, quantile)
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/query?query=%s", prometheusURL, query))
	if err != nil {
		return 1000.0
	}
	defer resp.Body.Close()
	var result struct {
		Data struct {
			Result []struct {
				Metric map[string]string
				Value  []interface{}
			}
		}
	}
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &result)

	for _, r := range result.Data.Result {
		if r.Metric["node"] == node {
			valStr := r.Value[1].(string)
			val, _ := strconv.ParseFloat(valStr, 64)
			return val
		}
	}
	return 1000.0 // fallback
}

func (r *Router) findBestNodes() []ScoredNode {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var scored []ScoredNode
	for addr, status := range r.nodeStatus {
		if !status.IsHealthy {
			continue
		}
		latency := getPrometheusQuantile(addr, 0.95)
		scored = append(scored, ScoredNode{
			Address: addr,
			Score:   latency,
		})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score < scored[j].Score
	})
	return scored
}

func (r *Router) updateNodeLatency(addr string, newLatency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.nodeStatus[addr]
	if status.Latency == 0 {
		status.Latency = newLatency
	} else {
		status.Latency = (status.Latency*7 + newLatency*3) / 10
	}
	queryLatency.WithLabelValues(addr).Observe(newLatency.Seconds())
}

func (r *Router) queryHandler(w http.ResponseWriter, req *http.Request) {
	requestBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	candidateNodes := r.findBestNodes()
	if len(candidateNodes) == 0 {
		http.Error(w, "No healthy backend nodes", http.StatusServiceUnavailable)
		return
	}

	for i := 0; i < len(candidateNodes) && i < maxRetries; i++ {
		target := candidateNodes[i].Address
		start := time.Now()
		conn, err := net.Dial("tcp", target)
		if err != nil {
			continue
		}
		defer conn.Close()

		_, err = conn.Write(requestBody)
		if err != nil {
			continue
		}

		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		resp := make([]byte, 2048)
		n, _ := conn.Read(resp)

		if n > 0 {
			w.Write(resp[:n])
		}

		lat := time.Since(start)
		r.updateNodeLatency(target, lat)
		requestsTotal.WithLabelValues(target).Inc()
		return
	}

	http.Error(w, "All retries failed", http.StatusServiceUnavailable)
}

func (r *Router) statusHandler(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	prettyStatus := make(map[string]map[string]interface{})
	for addr, status := range r.nodeStatus {
		prettyStatus[addr] = map[string]interface{}{
			"healthy":    status.IsHealthy,
			"latency_ms": status.Latency.Milliseconds(),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(prettyStatus)
}

func main() {
	var err error
	logger, err = zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	router := NewRouter()
	http.HandleFunc("/query", router.queryHandler)
	http.HandleFunc("/status", router.statusHandler)
	http.Handle("/metrics", promhttp.Handler())

	logger.Info("Smart Router started on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		logger.Fatal("Server failed", zap.Error(err))
	}
}
