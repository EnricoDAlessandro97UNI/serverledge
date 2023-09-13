package node

import (
	"container/list"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/grussorusso/serverledge/internal/types"

	"github.com/grussorusso/serverledge/internal/config"
	"github.com/grussorusso/serverledge/internal/container"
	"github.com/grussorusso/serverledge/internal/function"
)

type ContainerPool struct {
	busy  *list.List // list of ContainerID
	ready *list.List // list of warmContainer
}

type warmContainer struct {
	Expiration int64
	Function   string
	contID     container.ContainerID
}

type busyContainer struct {
	Function string
	contID   container.ContainerID
}

var NoWarmFoundErr = errors.New("no warm container is available")

// getFunctionPool retrieves (or creates) the container pool for a function.
func getFunctionPool(f *function.Function) *ContainerPool {
	if fp, ok := Resources.ContainerPools[f.Name]; ok {
		return fp
	}

	fp := newFunctionPool()
	Resources.ContainerPools[f.Name] = fp
	return fp
}

func (fp *ContainerPool) getWarmContainer(funcName string) (container.ContainerID, bool) {
	// TODO: picking most-recent / least-recent container might be better?
	elem := fp.ready.Front()
	if elem == nil {
		return "", false
	}

	if elem.Value.(warmContainer).Function != funcName {
		return "no function", false
	}

	fp.ready.Remove(elem)
	contID := elem.Value.(warmContainer).contID
	fp.putBusyContainer(contID, funcName)

	return contID, true
}

func (fp *ContainerPool) putBusyContainer(contID container.ContainerID, funcName string) {
	fmt.Printf("storing in the busy pool the container %s for func '%s'\n", contID, funcName)
	fp.busy.PushBack(busyContainer{
		Function: funcName,
		contID:   contID,
	})
}

func (fp *ContainerPool) putReadyContainer(contID container.ContainerID, funcName string, expiration int64) {
	fmt.Printf("storing in the ready pool warm container %s for func '%s'\n", contID, funcName)
	fp.ready.PushBack(warmContainer{
		contID:     contID,
		Function:   funcName, // FIXME this is wrong sometimes (multithreading)
		Expiration: expiration,
	})
}

func newFunctionPool() *ContainerPool {
	fp := &ContainerPool{}
	fp.busy = list.New()
	fp.ready = list.New()

	return fp
}

// AcquireResources reserves the specified amount of cpu and memory if possible.
func AcquireResources(cpuDemand float64, memDemand int64, destroyContainersIfNeeded bool) bool {
	Resources.Lock()
	defer Resources.Unlock()
	return acquireResources(cpuDemand, memDemand, destroyContainersIfNeeded)
}

// acquireResources reserves the specified amount of cpu and memory if possible.
// The function is NOT thread-safe.
func acquireResources(cpuDemand float64, memDemand int64, destroyContainersIfNeeded bool) bool {
	if Resources.AvailableCPUs < cpuDemand {
		return false
	}
	if Resources.AvailableMemMB < memDemand {
		if !destroyContainersIfNeeded {
			return false
		}

		enoughMem, _ := dismissContainer(memDemand)
		if !enoughMem {
			return false
		}
	}

	Resources.AvailableCPUs -= cpuDemand
	Resources.AvailableMemMB -= memDemand

	return true
}

// releaseResources releases the specified amount of cpu and memory.
// The function is NOT thread-safe.
func releaseResources(cpuDemand float64, memDemand int64) {
	Resources.AvailableCPUs += cpuDemand
	Resources.AvailableMemMB += memDemand
}

// AcquireWarmContainer acquires a warm container for a given function (if any).
// A warm container is in running/paused state and has already been initialized
// with the function code.
// The acquired container is already in the busy pool.
// The function returns an error if either:
// (i) the warm container does not exist
// (ii) there are not enough resources to start the container
func AcquireWarmContainer(f *function.Function) (container.ContainerID, error) {
	Resources.Lock()
	defer Resources.Unlock()

	fp := getFunctionPool(f)
	// fmt.Printf("ready containers: %+v\nbusy containers: %+v\n", fp.ready.Len(), fp.busy.Len())
	contID, found := fp.getWarmContainer(f.Name)
	if !found {
		if contID == "no function" {
			return "", fmt.Errorf("the container exists, but doesn't have the function %s", f.Name)
		}
		return "", NoWarmFoundErr
	}

	if !acquireResources(f.CPUDemand, 0, false) {
		//log.Printf("Not enough CPU to start a warm container for %s", f)
		return "", OutOfResourcesErr
	}

	//log.Printf("Acquired resources for warm container. Now: %v", Resources)
	return contID, nil
}

// ReleaseContainer puts a container in the ready pool for a function.
func ReleaseContainer(contID container.ContainerID, f *function.Function) { // TODO: questa funzione andrebbe eseguita prima di eseguire SubmitRequest
	// setup Expiration as time duration from now
	d := time.Duration(config.GetInt(config.CONTAINER_EXPIRATION_TIME, 600)) * time.Second
	expTime := time.Now().Add(d).UnixNano()

	Resources.Lock()
	defer Resources.Unlock()
	// fmt.Printf("getting function pool for function %s\n", f.Name)
	fp := getFunctionPool(f)
	fName := f.Name
	// we must update the busy list by removing this element
	elem := fp.busy.Front()
	for ok := elem != nil; ok; ok = elem != nil {
		if elem.Value.(busyContainer).contID == contID {
			fp.busy.Remove(elem) // delete the element from the busy list
			fName = elem.Value.(busyContainer).Function
			break
		}
		elem = elem.Next()
	}

	fp.putReadyContainer(contID, fName, expTime) // FIXME: passare la funzione giusta, l'ultima che è stata eseguita

	releaseResources(f.CPUDemand, 0)
	go func() {
		// fmt.Println("Sending ok after putting ready container")
		types.NodeDoneChan <- "ok"
		// fmt.Printf("Released resources. Now: %v\n", &Resources)
	}()
}

// NewContainer creates and starts a new container for the given function.
// The container can be directly used to schedule a request, as it is already
// in the busy pool.
func NewContainer(fun *function.Function) (container.ContainerID, error) {
	Resources.Lock()
	if !acquireResources(fun.CPUDemand, fun.MemoryMB, true) {
		//log.Printf("Not enough resources for the new container.")
		Resources.Unlock()
		return "", OutOfResourcesErr
	}

	//log.Printf("Acquired resources for new container. Now: %v", Resources)
	Resources.Unlock()

	return NewContainerWithAcquiredResources(fun)
}

// NewContainerWithAcquiredResources spawns a new container for the given
// function, assuming that the required CPU and memory resources have been
// already been acquired.
func NewContainerWithAcquiredResources(fun *function.Function) (container.ContainerID, error) {
	var image string
	if fun.Runtime == container.CUSTOM_RUNTIME {
		image = fun.CustomImage
	} else {
		runtime, ok := container.RuntimeToInfo[fun.Runtime]
		if !ok {
			log.Printf("Unknown runtime: %s\n", fun.Runtime)
			return "", fmt.Errorf("Invalid runtime: %s", fun.Runtime)
		}
		image = runtime.Image
	}

	contID, err := container.NewContainer(image, fun.TarFunctionCode, &container.ContainerOptions{
		MemoryMB: fun.MemoryMB,
		CPUQuota: fun.CPUDemand,
	})

	if err != nil {
		log.Printf("Failed container creation: %v\n", err)
	}

	Resources.Lock()
	defer Resources.Unlock()
	if err != nil {
		releaseResources(fun.CPUDemand, fun.MemoryMB)
		return "", err
	}

	fp := getFunctionPool(fun)
	fp.putBusyContainer(contID, fun.Name) // We immediately mark it as busy

	return contID, nil
}

type itemToDismiss struct {
	contID container.ContainerID
	pool   *ContainerPool
	elem   *list.Element
	memory int64
}

// dismissContainer ... this function is used to get free memory used for a new container
// 2-phases: first, we find ready container and collect them as a slice, second (cleanup phase) we delete the container only and only if
// the sum of their memory is >= requiredMemoryMB is
func dismissContainer(requiredMemoryMB int64) (bool, error) {
	var cleanedMB int64 = 0
	var containerToDismiss []itemToDismiss
	res := false

	//first phase, research
	for _, funPool := range Resources.ContainerPools {
		if funPool.ready.Len() > 0 {
			// every container into the funPool has the same memory (same function)
			//so it is not important which one you destroy
			elem := funPool.ready.Front()
			contID := elem.Value.(warmContainer).contID
			// container in the same pool need same memory
			memory, _ := container.GetMemoryMB(contID)
			for ok := true; ok; ok = elem != nil {
				containerToDismiss = append(containerToDismiss,
					itemToDismiss{contID: contID, pool: funPool, elem: elem, memory: memory})
				cleanedMB += memory
				if cleanedMB >= requiredMemoryMB {
					goto cleanup
				}
				//go on to the next one
				elem = elem.Next()
			}
		}
	}

cleanup: // second phase, cleanup
	// memory check
	if cleanedMB >= requiredMemoryMB {
		for _, item := range containerToDismiss {
			item.pool.ready.Remove(item.elem)     // remove the container from the funPool
			err := container.Destroy(item.contID) // destroy the container
			if err != nil {
				res = false
				return res, nil
			}
			Resources.AvailableMemMB += item.memory
		}

		res = true
	}
	return res, nil
}

// DeleteExpiredContainer is called by the container cleaner
// Deletes expired warm container
func DeleteExpiredContainer() {
	now := time.Now().UnixNano()

	Resources.Lock()
	defer Resources.Unlock()

	for _, pool := range Resources.ContainerPools {
		elem := pool.ready.Front()
		for ok := elem != nil; ok; ok = elem != nil {
			warmed := elem.Value.(warmContainer)
			if now > warmed.Expiration {
				temp := elem
				elem = elem.Next()
				log.Printf("cleaner: Removing container %s\n", warmed.contID)
				pool.ready.Remove(temp) // remove the expired element

				memory, _ := container.GetMemoryMB(warmed.contID)
				releaseResources(0, memory)
				err := container.Destroy(warmed.contID)
				if err != nil {
					log.Printf("Error while destroying container %s: %s\n", warmed.contID, err)
				}
				log.Printf("Released resources. Now: %v\n", &Resources)
			} else {
				elem = elem.Next()
			}
		}
	}

}

// ShutdownWarmContainersFor destroys warm containers of a given function
// Actual termination happens asynchronously.
func ShutdownWarmContainersFor(f *function.Function) {
	Resources.Lock()
	defer Resources.Unlock()

	fp, ok := Resources.ContainerPools[f.Name]
	if !ok {
		return
	}

	containersToDelete := make([]container.ContainerID, 0)

	elem := fp.ready.Front()
	for ok := elem != nil; ok; ok = elem != nil {
		warmed := elem.Value.(warmContainer)
		temp := elem
		elem = elem.Next()
		log.Printf("Removing container with ID %s\n", warmed.contID)
		fp.ready.Remove(temp)

		memory, _ := container.GetMemoryMB(warmed.contID)
		Resources.AvailableMemMB += memory
		containersToDelete = append(containersToDelete, warmed.contID)
	}

	go func(contIDs []container.ContainerID) {
		for _, contID := range contIDs {
			// No need to update available resources here
			if err := container.Destroy(contID); err != nil {
				log.Printf("An error occurred while deleting %s: %v\n", contID, err)
			} else {
				log.Printf("Deleted %s\n", contID)
			}
		}
	}(containersToDelete)
}

// ShutdownAllContainers destroys all container (usually on termination)
func ShutdownAllContainers() {
	Resources.Lock()
	defer Resources.Unlock()

	for fun, pool := range Resources.ContainerPools {
		elem := pool.ready.Front()
		for ok := elem != nil; ok; ok = elem != nil {
			warmed := elem.Value.(warmContainer)
			temp := elem
			elem = elem.Next()
			log.Printf("Removing container with ID %s\n", warmed.contID)
			pool.ready.Remove(temp)

			memory, _ := container.GetMemoryMB(warmed.contID)
			err := container.Destroy(warmed.contID)
			if err != nil {
				log.Printf("Error while destroying container %s: %s", warmed.contID, err)
			}
			Resources.AvailableMemMB += memory
		}

		functionDescriptor, _ := function.GetFunction(fun)

		elem = pool.busy.Front()
		for ok := elem != nil; ok; ok = elem != nil {
			contID := elem.Value.(busyContainer).contID
			temp := elem
			elem = elem.Next()
			log.Printf("Removing container with ID %s\n", contID)
			pool.ready.Remove(temp)

			memory, errMem := container.GetMemoryMB(contID)
			if errMem != nil {
				log.Printf("failed to get memory from container %s before destroying it: %v", contID, errMem)
				continue
			}
			err := container.Destroy(contID)
			if err != nil {
				log.Printf("failed to destroy container %s: %v\n", contID, err)
				continue
			}
			Resources.AvailableMemMB += memory
			Resources.AvailableCPUs += functionDescriptor.CPUDemand
		}
	}
}

// WarmStatus foreach function returns the corresponding number of warm container available
func WarmStatus() map[string]int {
	Resources.RLock()
	defer Resources.RUnlock()
	warmPool := make(map[string]int)
	for funcName, pool := range Resources.ContainerPools {
		warmPool[funcName] = pool.ready.Len()
	}

	return warmPool
}
