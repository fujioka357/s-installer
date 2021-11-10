package pjstatussyncer

import (
	"context"
	"fmt"
	"reflect"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	prowv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	v1 "github.com/openshift/ci-tools/pkg/api/pullrequestpayloadqualification/v1"
	controllerutil "github.com/openshift/ci-tools/pkg/controller/util"
)

const (
	controllerName = "prowjob_status_syncer"
)

func AddToManager(mgr controllerruntime.Manager, ns string) error {
	ctrl, err := controller.New(controllerName, mgr, controller.Options{
		MaxConcurrentReconciles: 1,
		Reconciler: &reconciler{
			logger: logrus.WithField("controller", controllerName),

			client: mgr.GetClient(),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to construct controller: %w", err)
	}

	// Watch only on updates
	predicateFuncs := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return false },
		DeleteFunc: func(event.DeleteEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if _, ok := e.ObjectNew.GetLabels()[v1.PullRequestPayloadQualificationRunLabel]; !ok {
				return false
			}

			if e.ObjectNew.GetNamespace() != ns {
				return false
			}

			return true
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}

	if err := ctrl.Watch(source.NewKindWithCache(&prowv1.ProwJob{}, mgr.GetCache()), pjHandler(), predicateFuncs); err != nil {
		return fmt.Errorf("failed to create watch: %w", err)
	}

	return nil
}

func pjHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(o ctrlruntimeclient.Object) []reconcile.Request {
		pj, ok := o.(*prowv1.ProwJob)
		if !ok {
			logrus.WithField("type", fmt.Sprintf("%T", o)).Error("Got object that was not a ProwJob")
			return nil
		}

		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Namespace: pj.Namespace, Name: pj.Name}},
		}
	})
}

var _ reconcile.Reconciler = &reconciler{}

type reconciler struct {
	logger *logrus.Entry
	client ctrlruntimeclient.Client
}

func (r *reconciler) Reconcile(ctx context.Context, request controllerruntime.Request) (controllerruntime.Result, error) {
	log := r.logger.WithField("request", request.String())
	err := r.reconcile(ctx, log, request)
	if err != nil {
		log.WithError(err).Error("Reconciliation failed")
	}
	return reconcile.Result{}, controllerutil.SwallowIfTerminal(err)
}

func (r *reconciler) reconcile(ctx context.Context, log *logrus.Entry, req controllerruntime.Request) error {
	logger := log.WithField("namespace", req.Namespace).WithField("name", req.Name)
	logger.Info("Starting reconciliation")

	pj := &prowv1.ProwJob{}
	if err := r.client.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: req.Namespace, Name: req.Name}, pj); err != nil {
		return fmt.Errorf("failed to get the ProwJob: %s in namespace %s: %w", req.Name, req.Namespace, err)
	}

	prpqrName := pj.Labels[v1.PullRequestPayloadQualificationRunLabel]
	prpqr := &v1.PullRequestPayloadQualificationRun{}
	if err := r.client.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: req.Namespace, Name: prpqrName}, prpqr); err != nil {
		return fmt.Errorf("failed to get the PullRequestPayloadQualificationRun: %s in namespace %s: %w", prpqrName, req.Namespace, err)
	}
	for i, job := range prpqr.Status.Jobs {
		if job.ProwJob == pj.Name && !reflect.DeepEqual(pj.Status, job.Status) {
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				prpqr.Status.Jobs[i].Status = pj.Status

				logger.WithField("to_state", pj.Status.State).Info("Updating PullRequestPayloadQualificationRun...")
				if err := r.client.Update(ctx, prpqr); err != nil {
					return err
				}
				return nil
			}); err != nil {
				return fmt.Errorf("failed to update PullRequestPayloadQualificationRun %s: %w", prpqr.Name, err)
			}
		}
	}

	return nil
}
