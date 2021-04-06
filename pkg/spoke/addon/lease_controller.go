package addon

import (
	"context"
	"time"

	addonv1alpha1 "github.com/open-cluster-management/api/addon/v1alpha1"
	addonclient "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformerv1alpha1 "github.com/open-cluster-management/api/client/addon/informers/externalversions/addon/v1alpha1"
	addonlisterv1alpha1 "github.com/open-cluster-management/api/client/addon/listers/addon/v1alpha1"
	"github.com/open-cluster-management/registration/pkg/helpers"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/clock"
	coordinformers "k8s.io/client-go/informers/coordination/v1"
	coordlisters "k8s.io/client-go/listers/coordination/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	//TODO add this to ManagedClusterAddOn api
	addOnAvailableConditionType = "ManagedClusterAddOnConditionAvailable"
	//TODO add this to ManagedClusterAddOn api
	leaseDurationSeconds = 60
	leaseDurationTimes   = 5
)

// managedClusterAddOnLeaseController udpates managed cluster addons status on the hub cluster through watching the managed
// cluster status on the managed cluster.
type managedClusterAddOnLeaseController struct {
	clusterName string
	clock       clock.Clock
	addOnClient addonclient.Interface
	addOnLister addonlisterv1alpha1.ManagedClusterAddOnLister
	leaseLister coordlisters.LeaseLister
}

// NewManagedClusterAddOnLeaseController returns an instance of managedClusterAddOnLeaseController
func NewManagedClusterAddOnLeaseController(clusterName string,
	addOnClient addonclient.Interface,
	addOnInformer addoninformerv1alpha1.ManagedClusterAddOnInformer,
	leaseInformer coordinformers.LeaseInformer,
	resyncInterval time.Duration,
	recorder events.Recorder) factory.Controller {
	c := &managedClusterAddOnLeaseController{
		clusterName: clusterName,
		clock:       clock.RealClock{},
		addOnClient: addOnClient,
		addOnLister: addOnInformer.Lister(),
		leaseLister: leaseInformer.Lister(),
	}
	return factory.New().
		WithInformersQueueKeyFunc(c.queueKeyFunc, leaseInformer.Informer()).
		WithSync(c.sync).
		ResyncEvery(resyncInterval).
		ToController("ManagedClusterAddOnLeaseController", recorder)
}

func (c *managedClusterAddOnLeaseController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	queueKey := syncCtx.QueueKey()
	if queueKey == factory.DefaultQueueKey {
		addOns, err := c.addOnLister.ManagedClusterAddOns(c.clusterName).List(labels.Everything())
		if err != nil {
			return err
		}
		for _, addOn := range addOns {
			if _, err := getAddOnConfig(addOn); err != nil {
				// failed to get addon configuration, ignore it.
				continue
			}
			// enqueue the addon to reconcile
			syncCtx.Queue().Add(addOn)
		}
		return nil
	}

	addOnNamespace, addOnName, err := cache.SplitMetaNamespaceKey(queueKey)
	if err != nil {
		// queue key is bad format, ignore it.
		return nil
	}

	addOn, err := c.addOnLister.ManagedClusterAddOns(c.clusterName).Get(addOnName)
	if errors.IsNotFound(err) {
		// addon is not found, could be deleted, ignore it.
		return nil
	}
	if err != nil {
		return err
	}

	return c.syncSingle(ctx, addOnNamespace, addOn, syncCtx.Recorder())
}

func (c *managedClusterAddOnLeaseController) syncSingle(ctx context.Context,
	leaseNamespace string,
	addOn *addonv1alpha1.ManagedClusterAddOn,
	recorder events.Recorder) error {
	// addon lease name should be same with the addon name.
	observedLease, err := c.leaseLister.Leases(leaseNamespace).Get(addOn.Name)

	var condition metav1.Condition
	switch {
	case errors.IsNotFound(err):
		condition = metav1.Condition{
			Type:    addOnAvailableConditionType,
			Status:  metav1.ConditionUnknown,
			Reason:  "ManagedClusterAddOnLeaseNotFound",
			Message: "Managed cluster addon agent lease is not found.",
		}
	case err != nil:
		return err
	case err == nil:
		now := c.clock.Now()
		gracePeriod := time.Duration(leaseDurationTimes*leaseDurationSeconds) * time.Second
		if now.Before(observedLease.Spec.RenewTime.Add(gracePeriod)) {
			// the lease is constantly updated, update its addon status to available
			condition = metav1.Condition{
				Type:    addOnAvailableConditionType,
				Status:  metav1.ConditionTrue,
				Reason:  "ManagedClusterAddOnLeaseUpdated",
				Message: "Managed cluster addon agent updates its lease constantly.",
			}
			break
		}

		// the lease is not constantly updated, update its addon status to unavailable
		condition = metav1.Condition{
			Type:    addOnAvailableConditionType,
			Status:  metav1.ConditionFalse,
			Reason:  "ManagedClusterAddOnLeaseUpdateStopped",
			Message: "Managed cluster addon agent stopped updating its lease.",
		}
	}

	if meta.IsStatusConditionPresentAndEqual(addOn.Status.Conditions, condition.Type, condition.Status) {
		// addon status is not changed, do nothing
		return nil
	}

	_, updated, err := helpers.UpdateManagedClusterAddOnStatus(
		ctx,
		c.addOnClient,
		c.clusterName,
		addOn.Name,
		helpers.UpdateManagedClusterAddOnStatusFn(condition),
	)
	if err != nil {
		return err
	}
	if updated {
		recorder.Eventf("ManagedClusterAddOnStatusUpdated",
			"update managed cluster addon %q available condition to %q, due to its lease is not updated constantly",
			addOn.Name, condition.Status)
	}

	return nil
}

func (c *managedClusterAddOnLeaseController) queueKeyFunc(lease runtime.Object) string {
	accessor, _ := meta.Accessor(lease)

	name := accessor.GetName()
	// addon lease name should be same with the addon name.
	addOn, err := c.addOnLister.ManagedClusterAddOns(c.clusterName).Get(name)
	if err != nil {
		// failed to get addon from hub, ignore this reconciliation.
		return ""
	}

	addOnConifg, err := getAddOnConfig(addOn)
	if err != nil {
		// failed to get addon configuration, ignore it.
		return ""
	}

	namespace := accessor.GetNamespace()
	if namespace != addOnConifg.InstallationNamespace {
		// the lease namesapce is not same with its addon installation namespace, ignore it.
		return ""
	}

	return namespace + "/" + name
}
