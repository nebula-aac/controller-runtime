/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/util/workqueue"

	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/internal/controller/metrics"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Options are the arguments for creating a new Controller.
type Options[request comparable] struct {
	// Reconciler is a function that can be called at any time with the Name / Namespace of an object and
	// ensures that the state of the system matches the state specified in the object.
	// Defaults to the DefaultReconcileFunc.
	Do reconcile.TypedReconciler[request]

	// RateLimiter is used to limit how frequently requests may be queued into the work queue.
	RateLimiter workqueue.TypedRateLimiter[request]

	// NewQueue constructs the queue for this controller once the controller is ready to start.
	// This is a func because the standard Kubernetes work queues start themselves immediately, which
	// leads to goroutine leaks if something calls controller.New repeatedly.
	NewQueue func(controllerName string, rateLimiter workqueue.TypedRateLimiter[request]) workqueue.TypedRateLimitingInterface[request]

	// MaxConcurrentReconciles is the maximum number of concurrent Reconciles which can be run. Defaults to 1.
	MaxConcurrentReconciles int

	// CacheSyncTimeout refers to the time limit set on waiting for cache to sync
	// Defaults to 2 minutes if not set.
	CacheSyncTimeout time.Duration

	// Name is used to uniquely identify a Controller in tracing, logging and monitoring.  Name is required.
	Name string

	// LogConstructor is used to construct a logger to then log messages to users during reconciliation,
	// or for example when a watch is started.
	// Note: LogConstructor has to be able to handle nil requests as we are also using it
	// outside the context of a reconciliation.
	LogConstructor func(request *request) logr.Logger

	// RecoverPanic indicates whether the panic caused by reconcile should be recovered.
	// Defaults to true.
	RecoverPanic *bool

	// LeaderElected indicates whether the controller is leader elected or always running.
	LeaderElected *bool

	// EnableWarmup specifies whether the controller should start its sources
	// when the manager is not the leader.
	// Defaults to false, which means that the controller will wait for leader election to start
	// before starting sources.
	EnableWarmup *bool
}

// Controller implements controller.Controller.
type Controller[request comparable] struct {
	// Name is used to uniquely identify a Controller in tracing, logging and monitoring.  Name is required.
	Name string

	// MaxConcurrentReconciles is the maximum number of concurrent Reconciles which can be run. Defaults to 1.
	MaxConcurrentReconciles int

	// Reconciler is a function that can be called at any time with the Name / Namespace of an object and
	// ensures that the state of the system matches the state specified in the object.
	// Defaults to the DefaultReconcileFunc.
	Do reconcile.TypedReconciler[request]

	// RateLimiter is used to limit how frequently requests may be queued into the work queue.
	RateLimiter workqueue.TypedRateLimiter[request]

	// NewQueue constructs the queue for this controller once the controller is ready to start.
	// This is a func because the standard Kubernetes work queues start themselves immediately, which
	// leads to goroutine leaks if something calls controller.New repeatedly.
	NewQueue func(controllerName string, rateLimiter workqueue.TypedRateLimiter[request]) workqueue.TypedRateLimitingInterface[request]

	// Queue is an listeningQueue that listens for events from Informers and adds object keys to
	// the Queue for processing
	Queue priorityqueue.PriorityQueue[request]

	// mu is used to synchronize Controller setup
	mu sync.Mutex

	// Started is true if the Controller has been Started
	Started bool

	// ctx is the context that was passed to Start() and used when starting watches.
	//
	// According to the docs, contexts should not be stored in a struct: https://golang.org/pkg/context,
	// while we usually always strive to follow best practices, we consider this a legacy case and it should
	// undergo a major refactoring and redesign to allow for context to not be stored in a struct.
	ctx context.Context

	// CacheSyncTimeout refers to the time limit set on waiting for cache to sync
	// Defaults to 2 minutes if not set.
	CacheSyncTimeout time.Duration

	// startWatches maintains a list of sources, handlers, and predicates to start when the controller is started.
	startWatches []source.TypedSource[request]

	// startedEventSourcesAndQueue is used to track if the event sources have been started.
	// It ensures that we append sources to c.startWatches only until we call Start() / Warmup()
	// It is true if startEventSourcesAndQueueLocked has been called at least once.
	startedEventSourcesAndQueue bool

	// didStartEventSourcesOnce is used to ensure that the event sources are only started once.
	didStartEventSourcesOnce sync.Once

	// LogConstructor is used to construct a logger to then log messages to users during reconciliation,
	// or for example when a watch is started.
	// Note: LogConstructor has to be able to handle nil requests as we are also using it
	// outside the context of a reconciliation.
	LogConstructor func(request *request) logr.Logger

	// RecoverPanic indicates whether the panic caused by reconcile should be recovered.
	// Defaults to true.
	RecoverPanic *bool

	// LeaderElected indicates whether the controller is leader elected or always running.
	LeaderElected *bool

	// EnableWarmup specifies whether the controller should start its sources when the manager is not
	// the leader. This is useful for cases where sources take a long time to start, as it allows
	// for the controller to warm up its caches even before it is elected as the leader. This
	// improves leadership failover time, as the caches will be prepopulated before the controller
	// transitions to be leader.
	//
	// Setting EnableWarmup to true and NeedLeaderElection to true means the controller will start its
	// sources without waiting to become leader.
	// Setting EnableWarmup to true and NeedLeaderElection to false is a no-op as controllers without
	// leader election do not wait on leader election to start their sources.
	// Defaults to false.
	EnableWarmup *bool
}

// New returns a new Controller configured with the given options.
func New[request comparable](options Options[request]) *Controller[request] {
	return &Controller[request]{
		Do:                      options.Do,
		RateLimiter:             options.RateLimiter,
		NewQueue:                options.NewQueue,
		MaxConcurrentReconciles: options.MaxConcurrentReconciles,
		CacheSyncTimeout:        options.CacheSyncTimeout,
		Name:                    options.Name,
		LogConstructor:          options.LogConstructor,
		RecoverPanic:            options.RecoverPanic,
		LeaderElected:           options.LeaderElected,
		EnableWarmup:            options.EnableWarmup,
	}
}

// Reconcile implements reconcile.Reconciler.
func (c *Controller[request]) Reconcile(ctx context.Context, req request) (_ reconcile.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			ctrlmetrics.ReconcilePanics.WithLabelValues(c.Name).Inc()

			if c.RecoverPanic == nil || *c.RecoverPanic {
				for _, fn := range utilruntime.PanicHandlers {
					fn(ctx, r)
				}
				err = fmt.Errorf("panic: %v [recovered]", r)
				return
			}

			log := logf.FromContext(ctx)
			log.Info(fmt.Sprintf("Observed a panic in reconciler: %v", r))
			panic(r)
		}
	}()
	return c.Do.Reconcile(ctx, req)
}

// Watch implements controller.Controller.
func (c *Controller[request]) Watch(src source.TypedSource[request]) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Sources weren't started yet, store the watches locally and return.
	// These sources are going to be held until either Warmup() or Start(...) is called.
	if !c.startedEventSourcesAndQueue {
		c.startWatches = append(c.startWatches, src)
		return nil
	}

	c.LogConstructor(nil).Info("Starting EventSource", "source", src)
	return src.Start(c.ctx, c.Queue)
}

// NeedLeaderElection implements the manager.LeaderElectionRunnable interface.
func (c *Controller[request]) NeedLeaderElection() bool {
	if c.LeaderElected == nil {
		return true
	}
	return *c.LeaderElected
}

// Warmup implements the manager.WarmupRunnable interface.
func (c *Controller[request]) Warmup(ctx context.Context) error {
	if c.EnableWarmup == nil || !*c.EnableWarmup {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Set the ctx so later calls to watch use this internal context
	c.ctx = ctx

	return c.startEventSourcesAndQueueLocked(ctx)
}

// Start implements controller.Controller.
func (c *Controller[request]) Start(ctx context.Context) error {
	// use an IIFE to get proper lock handling
	// but lock outside to get proper handling of the queue shutdown
	c.mu.Lock()
	if c.Started {
		return errors.New("controller was started more than once. This is likely to be caused by being added to a manager multiple times")
	}

	c.initMetrics()

	// Set the internal context.
	c.ctx = ctx

	wg := &sync.WaitGroup{}
	err := func() error {
		defer c.mu.Unlock()

		// TODO(pwittrock): Reconsider HandleCrash
		defer utilruntime.HandleCrashWithLogger(c.LogConstructor(nil))

		// NB(directxman12): launch the sources *before* trying to wait for the
		// caches to sync so that they have a chance to register their intended
		// caches.
		if err := c.startEventSourcesAndQueueLocked(ctx); err != nil {
			return err
		}

		c.LogConstructor(nil).Info("Starting Controller")

		// Launch workers to process resources
		c.LogConstructor(nil).Info("Starting workers", "worker count", c.MaxConcurrentReconciles)
		wg.Add(c.MaxConcurrentReconciles)
		for i := 0; i < c.MaxConcurrentReconciles; i++ {
			go func() {
				defer wg.Done()
				// Run a worker thread that just dequeues items, processes them, and marks them done.
				// It enforces that the reconcileHandler is never invoked concurrently with the same object.
				for c.processNextWorkItem(ctx) {
				}
			}()
		}

		c.Started = true
		return nil
	}()
	if err != nil {
		return err
	}

	<-ctx.Done()
	c.LogConstructor(nil).Info("Shutdown signal received, waiting for all workers to finish")
	wg.Wait()
	c.LogConstructor(nil).Info("All workers finished")
	return nil
}

// startEventSourcesAndQueueLocked launches all the sources registered with this controller and waits
// for them to sync. It returns an error if any of the sources fail to start or sync.
func (c *Controller[request]) startEventSourcesAndQueueLocked(ctx context.Context) error {
	var retErr error

	c.didStartEventSourcesOnce.Do(func() {
		queue := c.NewQueue(c.Name, c.RateLimiter)
		if priorityQueue, isPriorityQueue := queue.(priorityqueue.PriorityQueue[request]); isPriorityQueue {
			c.Queue = priorityQueue
		} else {
			c.Queue = &priorityQueueWrapper[request]{TypedRateLimitingInterface: queue}
		}
		go func() {
			<-ctx.Done()
			c.Queue.ShutDown()
		}()

		errGroup := &errgroup.Group{}
		for _, watch := range c.startWatches {
			log := c.LogConstructor(nil)
			_, ok := watch.(interface {
				String() string
			})
			if !ok {
				log = log.WithValues("source", fmt.Sprintf("%T", watch))
			} else {
				log = log.WithValues("source", fmt.Sprintf("%s", watch))
			}
			didStartSyncingSource := &atomic.Bool{}
			errGroup.Go(func() error {
				// Use a timeout for starting and syncing the source to avoid silently
				// blocking startup indefinitely if it doesn't come up.
				sourceStartCtx, cancel := context.WithTimeout(ctx, c.CacheSyncTimeout)
				defer cancel()

				sourceStartErrChan := make(chan error, 1) // Buffer chan to not leak goroutine if we time out
				go func() {
					defer close(sourceStartErrChan)
					log.Info("Starting EventSource")

					if err := watch.Start(ctx, c.Queue); err != nil {
						sourceStartErrChan <- err
						return
					}
					syncingSource, ok := watch.(source.TypedSyncingSource[request])
					if !ok {
						return
					}
					didStartSyncingSource.Store(true)
					if err := syncingSource.WaitForSync(sourceStartCtx); err != nil {
						err := fmt.Errorf("failed to wait for %s caches to sync %v: %w", c.Name, syncingSource, err)
						log.Error(err, "Could not wait for Cache to sync")
						sourceStartErrChan <- err
					}
				}()

				select {
				case err := <-sourceStartErrChan:
					return err
				case <-sourceStartCtx.Done():
					if didStartSyncingSource.Load() { // We are racing with WaitForSync, wait for it to let it tell us what happened
						return <-sourceStartErrChan
					}
					if ctx.Err() != nil { // Don't return an error if the root context got cancelled
						return nil
					}
					return fmt.Errorf("timed out waiting for source %s to Start. Please ensure that its Start() method is non-blocking", watch)
				}
			})
		}
		retErr = errGroup.Wait()

		// All the watches have been started, we can reset the local slice.
		//
		// We should never hold watches more than necessary, each watch source can hold a backing cache,
		// which won't be garbage collected if we hold a reference to it.
		c.startWatches = nil

		// Mark event sources as started after resetting the startWatches slice so that watches from
		// a new Watch() call are immediately started.
		c.startedEventSourcesAndQueue = true
	})

	return retErr
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the reconcileHandler.
func (c *Controller[request]) processNextWorkItem(ctx context.Context) bool {
	obj, priority, shutdown := c.Queue.GetWithPriority()
	if shutdown {
		// Stop working
		return false
	}

	// We call Done here so the workqueue knows we have finished
	// processing this item. We also must remember to call Forget if we
	// do not want this work item being re-queued. For example, we do
	// not call Forget if a transient error occurs, instead the item is
	// put back on the workqueue and attempted again after a back-off
	// period.
	defer c.Queue.Done(obj)

	ctrlmetrics.ActiveWorkers.WithLabelValues(c.Name).Add(1)
	defer ctrlmetrics.ActiveWorkers.WithLabelValues(c.Name).Add(-1)

	c.reconcileHandler(ctx, obj, priority)
	return true
}

const (
	labelError        = "error"
	labelRequeueAfter = "requeue_after"
	labelRequeue      = "requeue"
	labelSuccess      = "success"
)

func (c *Controller[request]) initMetrics() {
	ctrlmetrics.ReconcileTotal.WithLabelValues(c.Name, labelError).Add(0)
	ctrlmetrics.ReconcileTotal.WithLabelValues(c.Name, labelRequeueAfter).Add(0)
	ctrlmetrics.ReconcileTotal.WithLabelValues(c.Name, labelRequeue).Add(0)
	ctrlmetrics.ReconcileTotal.WithLabelValues(c.Name, labelSuccess).Add(0)
	ctrlmetrics.ReconcileErrors.WithLabelValues(c.Name).Add(0)
	ctrlmetrics.TerminalReconcileErrors.WithLabelValues(c.Name).Add(0)
	ctrlmetrics.ReconcilePanics.WithLabelValues(c.Name).Add(0)
	ctrlmetrics.WorkerCount.WithLabelValues(c.Name).Set(float64(c.MaxConcurrentReconciles))
	ctrlmetrics.ActiveWorkers.WithLabelValues(c.Name).Set(0)
}

func (c *Controller[request]) reconcileHandler(ctx context.Context, req request, priority int) {
	// Update metrics after processing each item
	reconcileStartTS := time.Now()
	defer func() {
		c.updateMetrics(time.Since(reconcileStartTS))
	}()

	log := c.LogConstructor(&req)
	reconcileID := uuid.NewUUID()

	log = log.WithValues("reconcileID", reconcileID)
	ctx = logf.IntoContext(ctx, log)
	ctx = addReconcileID(ctx, reconcileID)

	// RunInformersAndControllers the syncHandler, passing it the Namespace/Name string of the
	// resource to be synced.
	log.V(5).Info("Reconciling")
	result, err := c.Reconcile(ctx, req)
	switch {
	case err != nil:
		if errors.Is(err, reconcile.TerminalError(nil)) {
			ctrlmetrics.TerminalReconcileErrors.WithLabelValues(c.Name).Inc()
		} else {
			c.Queue.AddWithOpts(priorityqueue.AddOpts{RateLimited: true, Priority: priority}, req)
		}
		ctrlmetrics.ReconcileErrors.WithLabelValues(c.Name).Inc()
		ctrlmetrics.ReconcileTotal.WithLabelValues(c.Name, labelError).Inc()
		if !result.IsZero() {
			log.Info("Warning: Reconciler returned both a non-zero result and a non-nil error. The result will always be ignored if the error is non-nil and the non-nil error causes requeuing with exponential backoff. For more details, see: https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile#Reconciler")
		}
		log.Error(err, "Reconciler error")
	case result.RequeueAfter > 0:
		log.V(5).Info(fmt.Sprintf("Reconcile done, requeueing after %s", result.RequeueAfter))
		// The result.RequeueAfter request will be lost, if it is returned
		// along with a non-nil error. But this is intended as
		// We need to drive to stable reconcile loops before queuing due
		// to result.RequestAfter
		c.Queue.Forget(req)
		c.Queue.AddWithOpts(priorityqueue.AddOpts{After: result.RequeueAfter, Priority: priority}, req)
		ctrlmetrics.ReconcileTotal.WithLabelValues(c.Name, labelRequeueAfter).Inc()
	case result.Requeue: //nolint: staticcheck // We have to handle it until it is removed
		log.V(5).Info("Reconcile done, requeueing")
		c.Queue.AddWithOpts(priorityqueue.AddOpts{RateLimited: true, Priority: priority}, req)
		ctrlmetrics.ReconcileTotal.WithLabelValues(c.Name, labelRequeue).Inc()
	default:
		log.V(5).Info("Reconcile successful")
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.Queue.Forget(req)
		ctrlmetrics.ReconcileTotal.WithLabelValues(c.Name, labelSuccess).Inc()
	}
}

// GetLogger returns this controller's logger.
func (c *Controller[request]) GetLogger() logr.Logger {
	return c.LogConstructor(nil)
}

// updateMetrics updates prometheus metrics within the controller.
func (c *Controller[request]) updateMetrics(reconcileTime time.Duration) {
	ctrlmetrics.ReconcileTime.WithLabelValues(c.Name).Observe(reconcileTime.Seconds())
}

// ReconcileIDFromContext gets the reconcileID from the current context.
func ReconcileIDFromContext(ctx context.Context) types.UID {
	r, ok := ctx.Value(reconcileIDKey{}).(types.UID)
	if !ok {
		return ""
	}

	return r
}

// reconcileIDKey is a context.Context Value key. Its associated value should
// be a types.UID.
type reconcileIDKey struct{}

func addReconcileID(ctx context.Context, reconcileID types.UID) context.Context {
	return context.WithValue(ctx, reconcileIDKey{}, reconcileID)
}

type priorityQueueWrapper[request comparable] struct {
	workqueue.TypedRateLimitingInterface[request]
}

func (p *priorityQueueWrapper[request]) AddWithOpts(opts priorityqueue.AddOpts, items ...request) {
	for _, item := range items {
		switch {
		case opts.RateLimited:
			p.TypedRateLimitingInterface.AddRateLimited(item)
		case opts.After > 0:
			p.TypedRateLimitingInterface.AddAfter(item, opts.After)
		default:
			p.TypedRateLimitingInterface.Add(item)
		}
	}
}

func (p *priorityQueueWrapper[request]) GetWithPriority() (request, int, bool) {
	item, shutdown := p.TypedRateLimitingInterface.Get()
	return item, 0, shutdown
}
