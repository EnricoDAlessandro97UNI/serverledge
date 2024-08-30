package lb

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/grussorusso/serverledge/internal/config"
	"github.com/grussorusso/serverledge/internal/function"
	"github.com/grussorusso/serverledge/internal/lbcommon"
	"github.com/grussorusso/serverledge/internal/mab"
	"github.com/grussorusso/serverledge/internal/registration"
	"github.com/labstack/echo/v4"
)

var rwLock sync.RWMutex // rwLock is used to control concurrent access to a shared LBProxy data structure

// httpClient is a global HTTP client that can be reused for all requests.
// It is safe for concurrent use by multiple goroutines.
var client = &http.Client{}

// registerTerminationHandler sets up a signal handler to gracefully terminate the server
// when an interrupt signal (e.g., SIGINT) is received. It deregisters the server from the
// registration service (e.g., etcd), shuts down the Echo server with a 10-second timeout,
// and exits the program. The handler runs in a separate goroutine to listen for the signal
// and perform the termination tasks.
func registerTerminationHandler(r *registration.Registry, e *echo.Echo) {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)

	go func() {
		select {
		case sig := <-c:
			fmt.Printf("%s Got %s signal. Terminating...\n", LB, sig)

			// deregister from etcd; server should be unreachable
			err := r.Deregister()
			if err != nil {
				log.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := e.Shutdown(ctx); err != nil {
				e.Logger.Fatal(err)
			}

			os.Exit(0)
		}
	}()
}

// UpdateLBPolicy sets a new load balancing policy for the LBProxy.
// It updates the internal policy to the provided LBPolicy.
func (lbP *LBProxy) UpdateLBPolicy(lbPolicy LBPolicy) {
	lbP.lbPolicy = lbPolicy
	lbP.newStats.LBPolicy = lbP.lbPolicyName
}

// UpdateTargets sets the list of backend targets for the LBProxy.
// It updates the internal target list with the provided slice of URLs.
func (lbP *LBProxy) UpdateTargets(targets []*url.URL) {
	lbP.targetsInfo.targets = targets
}

// SelectBackend selects and returns a backend target URL based on the current load balancing policy.
// It acquires a read lock to safely access shared data, then releases the lock after selecting the target.
func (lbP *LBProxy) SelectBackend(funName string) *url.URL {
	rwLock.RLock()
	defer rwLock.RUnlock()
	return lbP.lbPolicy.SelectTarget(funName)
}

// HandleRequest processes an incoming HTTP request by selecting a backend server,
// forwarding the request to the chosen backend, and returning the backend's response
// to the client. It creates a new HTTP request with the same method, URI, and headers
// as the original request, then handles the response by copying headers and body
// to the client.
func (lbP *LBProxy) HandleRequest(c echo.Context) error {

	// Select backend
	funName := strings.TrimPrefix(c.Request().RequestURI, "/invoke/")
	backend := lbP.SelectBackend(funName)

	// Create a new HTTP request to forward to the selected backend
	req, err := http.NewRequest(c.Request().Method, backend.String()+c.Request().RequestURI, c.Request().Body)
	if err != nil {
		return err
	}
	// Copy the request headers to the new request
	req.Header = c.Request().Header

	// Send the request to the backend using the global HTTP client
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Read the response body from the backend
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("%s Error reading response body: %v", LB, err)
	}

	// If the request is an "invoke" request, process the response further
	if strings.HasPrefix(c.Request().RequestURI, "/invoke/") {
		// Check if the response status code is OK
		if resp.StatusCode == http.StatusOK {

			var executionReport function.ExecutionReport

			// Decode the JSON response body into an ExecutionReport structure
			err = json.Unmarshal(body, &executionReport)
			if err != nil {
				log.Fatalf("%s Error decoding JSON: %v", LB, err)
			}

			// Update statistics with the execution report
			rwLock.Lock()
			updateStats(lbP, executionReport, backend.String(), false)
			rwLock.Unlock()
		} else {
			// Update statistics to indicate a failed request
			rwLock.Lock()
			updateStats(lbP, function.ExecutionReport{}, backend.String(), true)
			rwLock.Unlock()
		}
	}

	// Copy the response headers from the backend to the client response
	for k, v := range resp.Header {
		c.Response().Header().Set(k, v[0])
	}

	// Set the response status code
	c.Response().WriteHeader(resp.StatusCode)

	// Copy the response body and send it to the client
	_, err = c.Response().Writer.Write(body)
	return err
}

// StartReverseProxy initializes and starts a reverse proxy server with load balancing capabilities.
// It retrieves the list of targets from a registry, initializes the load balancer proxy with a default
// policy (Random), sets up the Echo web server, and configures request handling. The function also
// starts background goroutines for updating targets and, if enabled, for running a Multi-Armed Bandit
// (MAB) agent to manage load balancing policies. Finally, it starts the server on a configured port.
func StartReverseProxy(r *registration.Registry, region string) {
	targets, err := getTargets(region)
	if err != nil {
		log.Fatalf("%s Cannot connect to registry to retrieve targets: %v", LB, err)
	}
	log.Println(LB, "Initializing with", len(targets), "targets:", targets)

	// Inizializza il proxy con la politica di default (random)
	lbProxy := &LBProxy{}
	lbProxy.targetsInfo = &TargetsInfo{}
	lbProxy.targetsInfo.targets = targets
	updateTargetsInfo(lbProxy, targets)
	lbProxy.lbPolicyName = lbcommon.Random
	lbProxy.lbPolicy = getLBPolicy(lbProxy.lbPolicyName, lbProxy)
	lbProxy.oldStats = newStats(lbProxy.lbPolicyName, lbProxy.targetsInfo.targets)
	lbProxy.newStats = newStats(lbProxy.lbPolicyName, lbProxy.targetsInfo.targets)

	e := echo.New()
	e.HideBanner = true
	e.Any("/*", lbProxy.HandleRequest)
	registerTerminationHandler(r, e)

	// Start the goroutine that periodically retrieves the available targets
	go updateTargets(lbProxy, region)

	// If enabled in the configuration file, start the MAB agent goroutine
	isMabAgentEnabled := config.GetBool(config.MAB_AGENT_ENABLED, false)
	if isMabAgentEnabled {
		log.Println(lbcommon.MAB, "MAB agent enabled")
		go startMABAgent(lbProxy)
	} else {
		log.Println(lbcommon.MAB, "MAB agent not enabled")
	}

	portNumber := config.GetInt(config.API_PORT, 1323)
	log.Printf("%s Starting LBProxy sever on port %d", LB, portNumber)
	if err := e.Start(fmt.Sprintf(":%d", portNumber)); err != nil && err != http.ErrServerClosed {
		e.Logger.Fatal(LB, "Shutting down the server")
	}
}

// updateTargets periodically retrieves and updates the list of backend targets from the registry.
// It runs in an infinite loop, checking for new targets every 30 seconds. If the list of targets has changed,
// it updates the targets in the LBProxy while holding a write lock to ensure thread-safe access.
func updateTargets(lbProxy *LBProxy, region string) {
	for {
		time.Sleep(3 * time.Second)
		targets, err := getTargets(region)
		if err != nil {
			log.Fatalf("%s Cannot connect to registry to retrieve targets: %v", LB, err)
		}
		if !compareURLTargets(lbProxy.targetsInfo.targets, targets) {
			rwLock.Lock()
			lbProxy.UpdateTargets(targets)
			updateTargetsInfo(lbProxy, targets)
			rwLock.Unlock()
		} else {
			rwLock.Lock()
			updateTargetsInfo(lbProxy, targets)
			rwLock.Unlock()
		}
	}
}

func updateTargetsInfo(lbP *LBProxy, targets []*url.URL) {
	// Retrieve status information for all nodes
	var targetsStatus []*registration.StatusInformation
	for _, node := range targets {
		statusInfo := getTargetStatus(node)
		if statusInfo != nil {
			targetsStatus = append(targetsStatus, statusInfo)
		} else {
			log.Fatalf("%s Error while getting status information", LB)
		}
	}
	lbP.targetsInfo.targetsStatus = append(lbP.targetsInfo.targetsStatus, targetsStatus...)
}

// Helper function to retrieve node status information via HTTP
func getTargetStatus(node *url.URL) *registration.StatusInformation {
	resp, err := http.Get(node.String() + "/status")
	if err != nil {
		log.Fatalf("%s Invocation to get status failed: %v", LB, err)
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("%s Error reading response body: %v", LB, err)
	}

	// Check the status code
	if resp.StatusCode == http.StatusOK {
		var statusInfo registration.StatusInformation
		if err := json.Unmarshal(body, &statusInfo); err != nil {
			log.Fatalf("%s Error decoding JSON: %v", LB, err)
		}
		return &statusInfo
	}

	return nil
}

// startMABAgent initializes and continuously runs a Multi-Armed Bandit (MAB) agent
// to periodically update load balancing policies based on the latest statistics.
// The agent operates at intervals specified by the configuration. It acquires a
// read-write lock to safely update its state and select the best policy, which
// is then applied to the load balancer. The function runs in an infinite loop.
func startMABAgent(lbProxy *LBProxy) {

	// MAB agent interval
	mabInterval := config.GetInt(config.MAB_AGENT_INTERVAL, 300)

	// Create the agent
	mabAgent := mab.NewMABAgent()
	log.Println(lbcommon.MAB, "MAB agent created")

	// Start the logic of the agent
	for {
		time.Sleep(time.Duration(mabInterval) * time.Second)

		log.Println(lbcommon.MAB, "MAB agent in action")

		// Acquire rwLock
		rwLock.Lock()

		// Update the agent with the stats
		mabAgent.Update(*lbProxy.newStats, *lbProxy.oldStats)

		// Save newStats in oldStats for future differences
		copyStats(lbProxy.newStats, lbProxy.oldStats)

		// Get the best policy according to the current strategy
		bestPolicy := mabAgent.SelectPolicy()

		// Use the selected policy
		lbProxy.UpdateLBPolicy(getLBPolicy(bestPolicy, lbProxy))

		// Release rwLock
		rwLock.Unlock()
	}
}
