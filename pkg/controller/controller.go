/*
Copyright 2018 The Multicluster-Controller Authors.

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

// Package controller implements the controller pattern.
package controller // import "orcastack.io/common/watch-operator/pkg/controller"

import (
	"context"
	"github.com/wangguoyan/mc-operator/pkg/cluster"
	"github.com/wangguoyan/mc-operator/pkg/handler"
	"github.com/wangguoyan/mc-operator/pkg/manager"
	"github.com/wangguoyan/mc-operator/pkg/reconcile"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"log"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller implements the controller pattern.
// A Controller owns a client-go workqueue. Watch methods set up the queue to receive reconcile Requests,
// e.g., on resource CRUD events in a cluster. The Requests are processed by the user-provided Reconciler.
// A Controller can watch multiple resources in multiple clusters. It saves those clusters in a set,
// so the Manager knows which caches to start and sync before starting the Controller.
type Controller struct {
	reconciler reconcile.Reconciler
	clusters   []manager.Cache
	Options
}

// Options is used as an argument of New.
type Options struct {
	// JitterPeriod is the time to wait after an error to start working again.
	JitterPeriod time.Duration
	// MaxConcurrentReconciles is the number of concurrent control loops.
	// Use this if your Reconciler is slow, but thread safe.
	MaxConcurrentReconciles int
	// Queue can be used to override the default queue.
	Queue workqueue.RateLimitingInterface
	// Logger can be used to override the default logger.
	Logger *log.Logger
}

// New creates a new Controller.
func New(r reconcile.Reconciler, o Options) *Controller {
	c := &Controller{
		reconciler: r,
		clusters:   nil,
		Options:    o,
	}

	if c.JitterPeriod == 0 {
		c.JitterPeriod = 1 * time.Second
	}

	if c.MaxConcurrentReconciles <= 0 {
		c.MaxConcurrentReconciles = 1
	}

	if c.Queue == nil {
		c.Queue = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	}

	if c.Logger == nil {
		c.Logger = log.New(os.Stdout, "", log.Lshortfile)
	}

	return c
}

// WatchOptions is used as an argument of WatchResource methods to filter events *on the client side*.
// You can filter on the server side with cluster.Options.
type WatchOptions struct {
	Namespace          string
	Namespaces         []string
	LabelSelector      labels.Selector
	AnnotationSelector labels.Selector
	CustomizeFilter    func(obj interface{}) bool
	Predicates         []predicate.Predicate
}

func (o WatchOptions) Filter(obj interface{}) bool {
	objMeta, err := meta.Accessor(obj)
	if err != nil {
		// TODO: log
		return false
	}

	objNS := objMeta.GetNamespace()

	if o.Namespace != "" && o.Namespace != objNS {
		return false
	}

	if len(o.Namespaces) > 0 {
		found := false
		for _, ns := range o.Namespaces {
			if ns == objNS {
				found = true
			}
		}
		if !found {
			return false
		}
	}

	if o.LabelSelector != nil && !o.LabelSelector.Matches(labels.Set(objMeta.GetLabels())) {
		return false
	}

	if o.AnnotationSelector != nil && !o.AnnotationSelector.Matches(labels.Set(objMeta.GetAnnotations())) {
		return false
	}

	if o.CustomizeFilter != nil && !o.CustomizeFilter(obj) {
		return false
	}

	return true
}

func (c *Controller) WatchResourceReconcileOwner(ctx context.Context, cluster cluster.ClusterCache, groupVersionKind schema.GroupVersionKind, owner client.Object, ownerWatchOption WatchOptions) error {
	h := &handler.EnqueueRequestForOwner{Cluster: cluster, Queue: c.Queue, GroupVersionKind: groupVersionKind, Filter: ownerWatchOption.Filter, Predicates: ownerWatchOption.Predicates}
	return c.WatchResource(ctx, cluster, owner, h)
}

// WatchResourceReconcileObject configures the Controller to watch resources of the same Kind as objectType,
// in the specified cluster, generating reconcile Requests from the ClusterCache's context
// and the watched objects' namespaces and names.
func (c *Controller) WatchResourceReconcileObject(ctx context.Context, cluster cluster.ClusterCache, objectType client.Object, o WatchOptions) error {
	h := &handler.EnqueueRequestForObject{Cluster: cluster, Queue: c.Queue, Filter: o.Filter, Predicates: o.Predicates}
	return c.WatchResource(ctx, cluster, objectType, h)
}

// WatchResource configures the Controller to watch resources of the same Kind as objectType,
// in the specified cluster, generating reconcile Requests an arbitrary ResourceEventHandler.
func (c *Controller) WatchResource(ctx context.Context, cluster cluster.ClusterCache, objectType client.Object, h cache.ResourceEventHandler) error {
	c.clusters = append(c.clusters, cluster)
	return cluster.AddEventHandler(ctx, objectType, h)
}

// GetCaches gets the current set of clusters (which implement manager.Cache) watched by the Controller.
// Manager uses this to ensure the necessary caches are started and synced before it starts the Controller.
func (c *Controller) GetCaches() []manager.Cache {
	return c.clusters
}

// Start starts the Controller's control loops (as many as MaxConcurrentReconciles) in separate channels
// and blocks until an empty struct is sent to the stop channel.
func (c *Controller) Start(ctx context.Context) error {
	defer c.Queue.ShutDown()

	for i := 0; i < c.MaxConcurrentReconciles; i++ {
		go wait.Until(func() {
			for c.processNextWorkItem() {
			}
		}, c.JitterPeriod, ctx.Done())
	}

	<-ctx.Done()
	return nil
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.Queue.Get()
	if obj == nil {
		c.Queue.Forget(obj)
	}

	if shutdown {
		c.Logger.Print("Shutting down. Ignore work item and stop working.")
		return false
	}

	defer c.Queue.Done(obj)
	var req reconcile.Request
	var ok bool
	if req, ok = obj.(reconcile.Request); !ok {
		c.Logger.Print("Work item is not a Request. Ignore it. Next.")
		c.Queue.Forget(obj)
		return true
	}

	if result, err := c.reconciler.Reconcile(req); err != nil {
		c.Logger.Print(err)
		c.Logger.Print("Could not reconcile Request. Stop working.")
		c.Queue.AddRateLimited(req)
		return false
	} else if result.RequeueAfter > 0 {
		c.Queue.AddAfter(req, result.RequeueAfter)
		return true
	} else if result.Requeue {
		c.Queue.AddRateLimited(req)
		return true
	}

	c.Queue.Forget(obj)
	return true
}
