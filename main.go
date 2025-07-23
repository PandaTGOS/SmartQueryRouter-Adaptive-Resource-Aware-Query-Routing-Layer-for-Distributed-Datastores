package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// --- Configuration ---
// List of backend nodes the router will manage.
// Replace these with the actual addresses of your database nodes.
var backendNodes = []string{
	"localhost:9001",
	"localhost:9002",
	"localhost:9003",
}

// How often the health checker runs.
const healthCheckInterval = 5 * time.Second

// --- Core Structures ---

// NodeStatus holds the state of a single backend node.
type NodeStatus struct {
	IsHealthy bool          `json:"is_healthy"`
	Latency   time.Duration `json:"latency_ms"`
}

// Router manages the state and routing logic for all backend nodes.
type Router struct {
	// A map to store the status of each node.
	nodeStatus map[string]*NodeStatus
	// A mutex to safely handle concurrent access to the nodeStatus map.
	mu sync.RWMutex
}

// NewRouter initializes a new router and starts its health checker.
func NewRouter() *Router {
	r := &Router{
		nodeStatus: make(map[string]*NodeStatus),
	}

	// Initialize status for all nodes.
	for _, addr := range backendNodes {
		r.nodeStatus[addr] = &NodeStatus{
			IsHealthy: false,
			Latency:   0,
		}
	}

	// Start the background health checker.
	go r.startHealthChecker()

	return r
}

// --- Health Checking ---

// startHealthChecker runs a periodic check on all backend nodes.
func (r *Router) startHealthChecker() {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	// Run an initial check immediately.
	log.Println("Starting initial health checks...")
	r.checkAllNodes()

	for range ticker.C {
		log.Println("Running periodic health checks...")
		r.checkAllNodes()
	}
}

// checkAllNodes iterates through each backend node and checks its health.
func (r *Router) checkAllNodes() {
	var wg sync.WaitGroup
	for addr := range r.nodeStatus {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			// A simple TCP dial is used as a health check.
			conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
			r.mu.Lock()
			defer r.mu.Unlock()
			if err != nil {
				if r.nodeStatus[addr].IsHealthy {
					log.Printf("Node %s is now UNHEALTHY\n", addr)
				}
				r.nodeStatus[addr].IsHealthy = false
			} else {
				if !r.nodeStatus[addr].IsHealthy {
					log.Printf("Node %s is now HEALTHY\n", addr)
				}
				r.nodeStatus[addr].IsHealthy = true
				conn.Close()
			}
		}(addr)
	}
	wg.Wait()
}

// --- Routing Logic ---

// findBestNode selects the optimal backend node to route the query to.
// The strategy: find the healthy node with the lowest latency.
func (r *Router) findBestNode() (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var bestNode string
	// Set initial minimum latency to a very high value.
	minLatency := time.Hour 

	for addr, status := range r.nodeStatus {
		if status.IsHealthy && status.Latency < minLatency {
			minLatency = status.Latency
			bestNode = addr
		}
	}

	if bestNode == "" {
		return "", fmt.Errorf("no healthy backend nodes available")
	}

	return bestNode, nil
}

// updateNodeLatency updates the latency for a node using a moving average.
// This prevents single slow queries from skewing the metrics too much.
func (r *Router) updateNodeLatency(addr string, newLatency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	status := r.nodeStatus[addr]
	// If this is the first latency measurement, just set it.
	if status.Latency == 0 {
		status.Latency = newLatency
	} else {
		// Calculate a simple moving average (70% old, 30% new).
		status.Latency = (status.Latency * 7 + newLatency*3) / 10
	}
}

// --- HTTP Handlers ---

// queryHandler intercepts the client's query, finds the best node,
// and proxies the request.
func (r *Router) queryHandler(w http.ResponseWriter, req *http.Request) {
	// 1. Find the best node to send the request to.
	targetNode, err := r.findBestNode()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		log.Printf("ERROR: Could not handle query: %v", err)
		return
	}
	log.Printf("Routing query to best node: %s", targetNode)

	// 2. Measure the time taken to proxy the request.
	startTime := time.Now()

	// 3. Open a connection to the target backend node.
	backendConn, err := net.Dial("tcp", targetNode)
	if err != nil {
		http.Error(w, "Failed to connect to backend", http.StatusInternalServerError)
		log.Printf("ERROR: Failed to connect to backend %s: %v", targetNode, err)
		return
	}
	defer backendConn.Close()

	// 4. Copy the request body from the client to the backend.
	requestBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	_, err = backendConn.Write(requestBody)
	if err != nil {
		http.Error(w, "Failed to send data to backend", http.StatusInternalServerError)
		return
	}

	// --- THIS IS THE CORRECTED PART ---
	// 5. Read the response from the backend with a timeout.
	// We can't use io.Copy because it waits for an EOF that ncat may not send.
	// Instead, we read into a buffer with a deadline.
	
	// Set a deadline for reading the response.
	backendConn.SetReadDeadline(time.Now().Add(1 * time.Second))
	
	responseBuffer := make([]byte, 2048) // Create a buffer for the response.
	n, err := backendConn.Read(responseBuffer)
	if err != nil {
		// A timeout error is expected if the backend keeps the connection open.
		// We only care if it's an error other than a timeout.
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			log.Printf("WARN: Error reading from backend (this may be ok): %v", err)
		}
	}

	// Write the bytes we received to the client.
	if n > 0 {
		w.Write(responseBuffer[:n])
	}
	// --- END OF CORRECTION ---

	// 6. Calculate latency and update the node's status.
	latency := time.Since(startTime)
	r.updateNodeLatency(targetNode, latency)
	
	log.Printf("Request to %s completed in %v", targetNode, latency)
}

// statusHandler returns the current state of all backend nodes as JSON.
func (r *Router) statusHandler(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// For cleaner JSON output, we create a temporary map.
	prettyStatus := make(map[string]map[string]interface{})
	for addr, status := range r.nodeStatus {
		prettyStatus[addr] = map[string]interface{}{
			"healthy": status.IsHealthy,
			"latency_ms": status.Latency.Milliseconds(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(prettyStatus)
}

// --- Main Function ---

func main() {
	// Create and initialize the router.
	router := NewRouter()

	// Set up the HTTP server and handlers.
	http.HandleFunc("/query", router.queryHandler)
	http.HandleFunc("/status", router.statusHandler)

	log.Println("SmartQueryRouter MVP started on :8080")
	log.Println(" - Send queries to http://localhost:8080/query")
	log.Println(" - Check node status at http://localhost:8080/status")

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}