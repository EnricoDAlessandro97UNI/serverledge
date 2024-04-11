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
		return new(fc.Request)
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

	if r.Async {
		go scheduling.SubmitAsyncRequest(r, false)
		return c.JSON(http.StatusOK, function.AsyncResponse{ReqId: r.ReqId})
	}

	err = scheduling.SubmitRequest(r, false)

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
	// TODO: here we shoulo parse from YAML (AFCL)/JSON (Step Functions)
	err := json.NewDecoder(e.Request().Body).Decode(&comp)
	if err != nil && err != io.EOF {
		log.Printf("Could not parse request: %v", err)
		return err
	}

	_, ok := fc.GetFC(comp.Name) // TODO: we would need a system-wide lock here...
	if ok {
		log.Printf("Dropping request for already existing composition '%s'", comp.Name)
		return e.JSON(http.StatusConflict, "")
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

	_, ok := fc.GetFC(comp.Name) // TODO: we would need a system-wide lock here...
	if !ok {
		log.Printf("Dropping request for non existing function '%s'", comp.Name)
		return c.JSON(http.StatusNotFound, "")
	}

	log.Printf("New request: deleting %s", comp.Name)
	err = comp.Delete()
	if err != nil {
		log.Printf("Failed deletion: %v", err)
		return c.JSON(http.StatusServiceUnavailable, "")
	}

	names := ""
	for _, f := range comp.Functions {
		// Delete local warm containers
		node.ShutdownWarmContainersFor(f)
		names += f.Name + " "
	}

	response := struct{ Deleted string }{comp.Name + " - deleted functions: " + names}
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

	// TODO: check if it's ok to reuse InvocationRequest also for Function Composition
	var invocationRequest client.InvocationRequest
	err := json.NewDecoder(e.Request().Body).Decode(&invocationRequest)
	if err != nil && err != io.EOF {
		log.Printf("Could not parse request: %v", err)
		return fmt.Errorf("could not parse request: %v", err)
	}
	// gets a fc.Request from the pool goroutine-safe cache.
	r := compositionRequestsPool.Get().(*fc.Request) // A pointer *function.Request will be created if does not exists, otherwise removed from the pool
	defer compositionRequestsPool.Put(r)             // at the end of the function, the function.Request is added to the pool.
	r.Fc = funComp
	r.Params = invocationRequest.Params
	r.Arrival = time.Now()

	r.MaxRespT = invocationRequest.QoSMaxRespT
	r.CanDoOffloading = invocationRequest.CanDoOffloading
	r.Async = invocationRequest.Async
	r.ReqId = fmt.Sprintf("%v-%s%d", funComp, node.NodeIdentifier[len(node.NodeIdentifier)-5:], r.Arrival.Nanosecond())
	// init fields if possibly not overwritten later
	r.ExecReport.SchedAction = ""
	r.ExecReport.OffloadLatency = 0.0
	// TODO: CAPIRE SE FARLA ASYNC E/O SYNC!
	if r.Async {
		// go scheduling.SubmitAsyncRequest(r)
		return e.JSON(http.StatusOK, function.AsyncResponse{ReqId: r.ReqId})
	}

	// err = scheduling.SubmitRequest(r) // Fai partire la prima funzione, aspetta il completamento, e cosi' via

	if errors.Is(err, node.OutOfResourcesErr) {
		return e.String(http.StatusTooManyRequests, "")
	} else if err != nil {
		log.Printf("Invocation failed: %v", err)
		return e.String(http.StatusInternalServerError, "Node has not enough resources")
	} else {
		return e.JSON(http.StatusOK, function.Response{Success: true, ExecutionReport: r.ExecReport})
	}
}
