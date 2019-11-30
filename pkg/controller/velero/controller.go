package velero

import (
	"context"
	"fmt"
	"time"

	veleroCR "github.com/openshift/managed-velero-operator/pkg/apis/managed/v1alpha1"
	"github.com/openshift/managed-velero-operator/pkg/gcs"
	"github.com/openshift/managed-velero-operator/pkg/util/platform"

	velerov1 "github.com/heptio/velero/pkg/apis/velero/v1"
	minterv1 "github.com/openshift/cloud-credential-operator/pkg/apis/cloudcredential/v1"
	appsv1 "k8s.io/api/apps/v1"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	log                    = logf.Log.WithName("controller_velero")
	storageReconcilePeriod = 60 * time.Minute
)

// Add creates a new Velero Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileVelero{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("velero-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource, Velero
	err = c.Watch(&source.Kind{Type: &veleroCR.Velero{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to BackupStorageLocation
	err = c.Watch(&source.Kind{Type: &velerov1.BackupStorageLocation{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &veleroCR.Velero{},
	})
	if err != nil {
		return err
	}

	// Watch for changes to VolumeSnapshotLocation
	err = c.Watch(&source.Kind{Type: &velerov1.VolumeSnapshotLocation{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &veleroCR.Velero{},
	})
	if err != nil {
		return err
	}

	// Watch for changes to CredentialsRequest
	err = c.Watch(&source.Kind{Type: &minterv1.CredentialsRequest{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &veleroCR.Velero{},
	})
	if err != nil {
		return err
	}

	// Watch for changes to Deployments
	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &veleroCR.Velero{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileVelero implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileVelero{}

// ReconcileVelero reconciles a Velero object
type ReconcileVelero struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a Velero object and makes changes based on the state read
// and what is in the Velero.Spec
func (r *ReconcileVelero) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Velero Installation")
	var err error

	// Fetch the Velero instance
	instance := &veleroCR.Velero{}
	err = r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Grab platform status to determine where OpenShift is installed
	platformStatusClient, err := platform.GetPlatformStatusClient()
	if err != nil {
		return reconcile.Result{}, err
	}
	platformStatus, err := platform.GetPlatformStatus(platformStatusClient)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Create an GCS client based on the region we received
	gcsClient, err := gcs.NewGCSClient(r.client)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Check if bucket needs to be reconciled
	if instance.StorageBucketReconcileRequired(storageReconcilePeriod) {
		// Always directly return from this, as we will either update the
		// timestamp when complete, or return an error.
		return r.provisionStorage(reqLogger, gcsClient, platformStatus, instance)
	}

	// Now go provision Velero
	return r.provisionVelero(reqLogger, request.Namespace, platformStatus, instance)
}

func (r *ReconcileVelero) statusUpdate(reqLogger logr.Logger, instance *veleroCR.Velero) error {
	err := r.client.Status().Update(context.TODO(), instance)
	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("Status update for %s failed", instance.Name))
	} else {
		reqLogger.Info(fmt.Sprintf("Status updated for %s", instance.Name))
	}
	return err
}
