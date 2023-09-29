package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/grussorusso/serverledge/internal/fc"

	"github.com/grussorusso/serverledge/internal/client"
	"github.com/grussorusso/serverledge/internal/config"
	"github.com/grussorusso/serverledge/internal/container"
	"github.com/grussorusso/serverledge/internal/fc"
	"github.com/grussorusso/serverledge/internal/function"
	"github.com/grussorusso/serverledge/internal/node"
	"github.com/grussorusso/serverledge/internal/registration"
	"github.com/grussorusso/serverledge/utils"

	"github.com/grussorusso/serverledge/internal/scheduling"
	"github.com/labstack/echo/v4"
)

var requestsPool = sync.Pool{
	New: func() any {
		return new(function.Request)
	},
}

var compositionRequestsPool = sync.Pool{
	New: func() any {
		return new(fc.CompositionRequest)
	},
}

// GetFunctions handles a request to list the function available in the system.
func GetFunctions(c echo.Context) error {
	list, err := function.GetAll()
	if err != nil {
		return c.String(http.StatusServiceUnavailable, "")
	}
	return c.JSON(http.StatusOK, list)
}

// InvokeFunction handles a function invocation request.
func InvokeFunction(c echo.Context) error {
	funcName := c.Param("fun")
	fun, ok := function.GetFunction(funcName)
	if !ok {
		log.Printf("Dropping request for unknown fun '%s'\n", funcName)
		return c.JSON(http.StatusNotFound, "")
	}

	var invocationRequest client.InvocationRequest
	err := json.NewDecoder(c.Request().Body).Decode(&invocationRequest)
	if err != nil && err != io.EOF {
		log.Printf("Could not parse request: %v\n", err)
		return fmt.Errorf("could not parse request: %v", err)
	}
	// gets a function.Request from the pool goroutine-safe cache.
	r := requestsPool.Get().(*function.Request) // function.Request will be created if does not exists, otherwise removed from the pool
	defer requestsPool.Put(r)                   // at the end of the function, the function.Request is added to the pool.
	r.Fun = fun
	r.Params = invocationRequest.Params
	r.Arrival = time.Now()
	r.MaxRespT = invocationRequest.QoSMaxRespT
	r.CanDoOffloading = invocationRequest.CanDoOffloading
	r.Async = invocationRequest.Async
	r.ReqId = fmt.Sprintf("%s-%s%d", fun, node.NodeIdentifier[len(node.NodeIdentifier)-5:], r.Arrival.Nanosecond())
	// init fields if possibly not overwritten later
	r.ExecReport.SchedAction = ""
	r.ExecReport.OffloadLatency = 0.0
	r.IsInComposition = false

	if r.Async {
		go scheduling.SubmitAsyncRequest(r)
		return c.JSON(http.StatusOK, function.AsyncResponse{ReqId: r.ReqId})
	}

	err = scheduling.SubmitRequest(r)

	if errors.Is(err, node.OutOfResourcesErr) {
		return c.String(http.StatusTooManyRequests, "")
	} else if err != nil {
		log.Printf("Invocation failed: %v\n", err)
		return c.String(http.StatusInternalServerError, "Node has not enough resources")
	} else {
		return c.JSON(http.StatusOK, function.Response{Success: true, ExecutionReport: r.ExecReport})
	}
}

// PollAsyncResult checks for the result of an asynchronous invocation.
func PollAsyncResult(c echo.Context) error {
	reqId := c.Param("reqId")
	if len(reqId) < 0 {
		return c.JSON(http.StatusNotFound, "")
	}

	etcdClient, err := utils.GetEtcdClient()
	if err != nil {
		log.Println("Could not connect to Etcd")
		return c.JSON(http.StatusInternalServerError, "")
	}

	ctx := context.Background()

	key := fmt.Sprintf("async/%s", reqId)
	res, err := etcdClient.Get(ctx, key)
	if err != nil {
		log.Println(err)
		return c.JSON(http.StatusInternalServerError, "")
	}

	if len(res.Kvs) == 1 {
		payload := res.Kvs[0].Value
		return c.JSONBlob(http.StatusOK, payload)
	} else {
		return c.JSON(http.StatusNotFound, "function not found")
	}
}

// CreateFunction handles a function creation request.
func CreateFunction(c echo.Context) error {
	var f function.Function
	err := json.NewDecoder(c.Request().Body).Decode(&f)
	if err != nil && err != io.EOF {
		log.Printf("Could not parse request: %v\n", err)
		return err
	}

	_, ok := function.GetFunction(f.Name) // TODO: we would need a system-wide lock here...
	if ok {
		log.Printf("Dropping request for already existing function '%s'\n", f.Name)
		return c.JSON(http.StatusConflict, "")
	}

	log.Printf("New request: creation of %s\n", f.Name)

	// Check that the selected runtime exists
	if f.Runtime != container.CUSTOM_RUNTIME {
		_, ok := container.RuntimeToInfo[f.Runtime]
		if !ok {
			return c.JSON(http.StatusNotFound, "Invalid runtime.")
		}
	}

	err = f.SaveToEtcd()
	if err != nil {
		log.Printf("Failed creation: %v\n", err)
		return c.JSON(http.StatusServiceUnavailable, "")
	}
	response := struct{ Created string }{f.Name}
	return c.JSON(http.StatusOK, response)
}

// DeleteFunction handles a function deletion request.
func DeleteFunction(c echo.Context) error {
	var f function.Function
	err := json.NewDecoder(c.Request().Body).Decode(&f)
	if err != nil && err != io.EOF {
		log.Printf("Could not parse request: %v\n", err)
		return err
	}

	_, ok := function.GetFunction(f.Name) // TODO: we would need a system-wide lock here...
	if !ok {
		log.Printf("Dropping request for non existing function '%s'\n", f.Name)
		return c.JSON(http.StatusNotFound, "")
	}

	log.Printf("New request: deleting %s\n", f.Name)
	err = f.Delete()
	if err != nil {
		log.Printf("Failed deletion: %v\n", err)
		return c.JSON(http.StatusServiceUnavailable, "")
	}

	// Delete local warm containers
	node.ShutdownWarmContainersFor(&f)

	response := struct{ Deleted string }{f.Name}
	return c.JSON(http.StatusOK, response)
}

func DecodeServiceClass(serviceClass string) (p function.ServiceClass) {
	if serviceClass == "low" {
		return function.LOW
	} else if serviceClass == "performance" {
		return function.HIGH_PERFORMANCE
	} else if serviceClass == "availability" {
		return function.HIGH_AVAILABILITY
	} else {
		return function.LOW
	}
}

// GetServerStatus simple api to check the current server status
func GetServerStatus(c echo.Context) error {
	node.Resources.RLock()
	defer node.Resources.RUnlock()
	portNumber := config.GetInt("api.port", 1323)
	url := fmt.Sprintf("http://%s:%d", utils.GetIpAddress().String(), portNumber)
	response := registration.StatusInformation{
		Url:                     url,
		AvailableWarmContainers: node.WarmStatus(),
		AvailableMemMB:          node.Resources.AvailableMemMB,
		AvailableCPUs:           node.Resources.AvailableCPUs,
		DropCount:               node.Resources.DropCount,
		Coordinates:             *registration.Reg.Client.GetCoordinate(),
	}

	return c.JSON(http.StatusOK, response)
}

// ===== Function Composition =====

func CreateFunctionComposition(e echo.Context) error {
	var comp fc.FunctionComposition
	// here we expect to receive the function composition struct already parsed from JSON/YAML
	var body []byte
	body, errReadBody := io.ReadAll(e.Request().Body)
	if errReadBody != nil {
		return errReadBody
	}

	err := json.Unmarshal(body, &comp)
	if err != nil && err != io.EOF {
		log.Printf("Could not parse request: %v", err)
		return err
	}

	_, ok := fc.GetFC(comp.Name) // TODO: we would need a system-wide lock here...
	if ok {
		log.Printf("Dropping request for already existing composition '%s'", comp.Name)
		return e.JSON(http.StatusConflict, "composition already exists")
	}

	log.Printf("New request: creation of composition %s", comp.Name)

	for _, f := range comp.Functions {
		// Check that the selected runtime exists
		if f.Runtime != container.CUSTOM_RUNTIME {
			_, ok := container.RuntimeToInfo[f.Runtime]
			if !ok {
				return e.JSON(http.StatusNotFound, "Invalid runtime.")
			}
		}
	}

	err = comp.SaveToEtcd()
	if err != nil {
		log.Printf("Failed creation: %v", err)
		return e.JSON(http.StatusServiceUnavailable, "")
	}
	response := struct{ Created string }{comp.Name}
	return e.JSON(http.StatusOK, response)
}

// GetFunctionCompositions handles a request to list the function compositions available in the system.
func GetFunctionCompositions(c echo.Context) error {
	list, err := fc.GetAllFC()
	if err != nil {
		return c.String(http.StatusServiceUnavailable, "")
	}
	return c.JSON(http.StatusOK, list)
}

// DeleteFunctionComposition handles a function deletion request.
func DeleteFunctionComposition(c echo.Context) error {
	var comp fc.FunctionComposition
	// here we only need the name of the function composition (and if all function should be deleted with it)
	err := json.NewDecoder(c.Request().Body).Decode(&comp)
	if err != nil && err != io.EOF {
		log.Printf("Could not parse request: %v", err)
		return err
	}

	composition, ok := fc.GetFC(comp.Name) // TODO: we would need a system-wide lock here...
	if !ok {
		log.Printf("Dropping request for non existing function '%s'", comp.Name)
		return c.JSON(http.StatusNotFound, "the request function composition to delete does not exist")
	}
	// only if RemoveFnOnDeletion is true, we also remove functions and associated warm (idle) containers
	msg := ""
	if composition.RemoveFnOnDeletion {
		names := "["
		i := 0
		for _, f := range composition.Functions {
			// Delete local warm containers
			node.ShutdownWarmContainersFor(f)
			names += f.Name
			if i != len(composition.Functions)-1 {
				names += " "
			}
			i++
		}
		names += "]"
		msg = " - deleted functions: " + names
	}

	log.Printf("New request: deleting %s", composition.Name)
	err = composition.Delete()
	if err != nil {
		log.Printf("Failed deletion: %v", err)
		return c.JSON(http.StatusServiceUnavailable, "")
	}

	response := struct{ Deleted string }{composition.Name + msg}
	return c.JSON(http.StatusOK, response)
}

// InvokeFunctionComposition handles a function composition invocation request.
func InvokeFunctionComposition(e echo.Context) error {
	// gets the command line param value for -fc (the composition name)
	fcName := e.Param("fc")
	funComp, ok := fc.GetFC(fcName)
	if !ok {
		log.Printf("Dropping request for unknown FC '%s'", fcName)
		return e.JSON(http.StatusNotFound, "")
	}

	// we use invocation request that is specific to function compositions
	var fcInvocationRequest client.CompositionInvocationRequest
	err := json.NewDecoder(e.Request().Body).Decode(&fcInvocationRequest)
	if err != nil && err != io.EOF {
		log.Printf("Could not parse request: %v", err)
		return fmt.Errorf("could not parse request: %v", err)
	}
	// gets a fc.CompositionRequest from the pool goroutine-safe cache.
	fcReq := compositionRequestsPool.Get().(*fc.CompositionRequest) // A pointer *function.CompositionRequest will be created if does not exists, otherwise removed from the pool
	defer compositionRequestsPool.Put(fcReq)                        // at the end of the function, the function.CompositionRequest is added to the pool.
	fcReq.Fc = funComp
	fcReq.Params = fcInvocationRequest.Params
	fcReq.Arrival = time.Now()

	// instead of saving only one RequestQoS, we save a map with an entry for each function in the composition
	fcReq.RequestQoSMap = fcInvocationRequest.RequestQoSMap

	fcReq.CanDoOffloading = fcInvocationRequest.CanDoOffloading
	fcReq.Async = fcInvocationRequest.Async
	fcReq.ReqId = fmt.Sprintf("%v-%s%d", funComp.Name, node.NodeIdentifier[len(node.NodeIdentifier)-5:], fcReq.Arrival.Nanosecond())
	// init fields if possibly not overwritten later
	fcReq.ExecReport.Reports = make(map[fc.DagNodeId]*function.ExecutionReport)
	for nodeId := range funComp.Workflow.Nodes {
		fcReq.ExecReport.Reports[nodeId] = &function.ExecutionReport{}
		fcReq.ExecReport.Reports[nodeId].SchedAction = ""
		fcReq.ExecReport.Reports[nodeId].OffloadLatency = 0.0
	}

	if fcReq.Async {
		errChan := make(chan error)
		go func(fcReq *fc.CompositionRequest) {
			executionReport, errInvoke := funComp.Invoke(fcReq)
			if errInvoke != nil {
				errChan <- errInvoke
				return
			}
			errChan <- nil
			fcReq.ExecReport = executionReport
			fcReq.ExecReport.ResponseTime = time.Now().Sub(fcReq.Arrival).Seconds()
		}(fcReq)

		errAsyncInvoke := <-errChan // FIXME: forse non va bene bloccarsi qui

		if errAsyncInvoke != nil {
			log.Printf("Invocation failed: %v", errAsyncInvoke)
			return e.String(http.StatusInternalServerError, "Composition invocation failed")
		}

		return e.JSON(http.StatusOK, function.AsyncResponse{ReqId: fcReq.ReqId})
	}

	// err = scheduling.SubmitCompositionRequest(fcReq) // Fai partire la prima funzione, aspetta il completamento, e cosi' via
	// sync execution
	executionReport, err := funComp.Invoke(fcReq)
	if err != nil {
		log.Printf("Invocation failed: %v", err)
		return e.String(http.StatusInternalServerError, "Composition invocation failed")
	}
	fcReq.ExecReport = executionReport
	fcReq.ExecReport.ResponseTime = time.Now().Sub(fcReq.Arrival).Seconds()

	if errors.Is(err, node.OutOfResourcesErr) {
		return e.String(http.StatusTooManyRequests, "")
	} else if err != nil {
		log.Printf("Invocation failed: %v", err)
		return e.String(http.StatusInternalServerError, "Node has not enough resources")
	} else {
		return e.JSON(http.StatusOK, fc.CompositionResponse{Success: true, CompositionExecutionReport: fcReq.ExecReport})
	}
}
