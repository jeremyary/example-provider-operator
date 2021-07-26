/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/jeremyary/example-provider-operator/api/v1alpha1"
	v1 "k8s.io/api/apps/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/pointer"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"time"
)

// SelfDeploymentReconciler reconciles the deployment for this operator
type SelfDeploymentReconciler struct {
	client.Client
	*discovery.DiscoveryClient
	Log                      logr.Logger
	Scheme                   *runtime.Scheme
	operatorNameVersion      string
	operatorInstallNamespace string
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;create;update;delete;watch
// +kubebuilder:rbac:groups=resource.acme.com,resources=*,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=resource.acme.com,resources=*/status,verbs=get;update;patch

func (r *SelfDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("selfDeploymentReconcile", req.NamespacedName)

	// due to predicate filtering, we'll only reconcile this operator's own deployment when it's seen the first time
	// meaning we have a reconcile entry-point on operator start-up, so now we can create a cluster-scoped resource
	// owned by the operator's ClusterRole to ensure cleanup on uninstall

	dep := &v1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, dep); err != nil {
		if errors.IsNotFound(err) {
			// CR deleted since request queued, child objects getting GC'd, no requeue
			log.Info("deployment not found, deleted, no requeue")
			return ctrl.Result{}, nil
		}
		// error fetching deployment, requeue and try again
		log.Error(err, "error fetching Deployment CR")
		return ctrl.Result{}, err
	}

	// check to see if the target CRD is present and if not, requeue with growing interval
	//
	// if you want to test the exponential backoff of our custom requeue, just change these to something that
	// doesn't exist on cluster, like this:
	//found, err := r.checkCrdInstalled("not.real.com/v1alpha1", "Foo")
	//
	// if you want to test the rest of this logic, point to operator's own CRD, which has to exist:
	found, err := r.checkCrdInstalled("resource.acme.com/v1alpha1", "ClusterScopedResource")
	if err != nil {
		log.Error(err, "error discovering GVK")
		return ctrl.Result{}, err
	}
	if !found {
		log.Info("CRD not found, requeueing with rate limiter")
		// returning with 'Requeue: true' will invoke our custom rate limiter seen in SetupWithManager below
		return ctrl.Result{Requeue: true}, nil
	} else {

		obj := &v1alpha1.ClusterScopedResource{
			ObjectMeta: v12.ObjectMeta{
				Name: "example-resource",
			},
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if errors.IsNotFound(err) {
				// CR deleted since request queued, child objects getting GC'd, no requeue
				log.Info("resource not found, creating now")

				// our custom resource isn't present,so create now with ClusterRole owner for GC
				opts := &client.ListOptions{
					LabelSelector: labels.SelectorFromSet(map[string]string{
						"olm.owner":      r.operatorNameVersion,
						"olm.owner.kind": "ClusterServiceVersion",
					}),
				}
				clusterRoleList := &rbac.ClusterRoleList{}
				if err := r.List(context.Background(), clusterRoleList, opts); err != nil {
					log.Error(err, "unable to list ClusterRoles to seek potential operand owners")
					return ctrl.Result{}, err
				}

				if len(clusterRoleList.Items) < 1 {
					err := errors.NewNotFound(
						schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "ClusterRole"}, "potentialOwner")
					log.Error(err, "could not find ClusterRole owned by CSV to inherit operand")
					return ctrl.Result{}, err
				}

				obj = &v1alpha1.ClusterScopedResource{
					ObjectMeta: v12.ObjectMeta{
						Name: "example-resource",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "rbac.authorization.k8s.io/v1",
								Kind:               "ClusterRole",
								UID:                clusterRoleList.Items[0].GetUID(), // doesn't really matter which 'item' we use
								Name:               clusterRoleList.Items[0].Name,
								Controller:         pointer.BoolPtr(true),
								BlockOwnerDeletion: pointer.BoolPtr(false),
							},
						},
					},
					Spec: v1alpha1.ClusterScopedResourceSpec{
						Foo: "bar",
					},
				}
				if err := r.Create(ctx, obj); err != nil {
					log.Error(err, "error while creating new cluster-scoped resource")
					return ctrl.Result{}, err
				} else {
					log.Info("cluster-scoped resource created")
					return ctrl.Result{}, nil
				}
			}
			// error fetching the resource, requeue and try again
			log.Error(err, "error fetching resource CR")
			return ctrl.Result{}, err
		} else {
			log.Info("resource exists, skipping create")
			return ctrl.Result{}, nil
		}
	}
}

// checkCrdInstalled checks whether dbaas provider CRD, has been created yet
func (r *SelfDeploymentReconciler) checkCrdInstalled(groupVersion, kind string) (bool, error) {
	resources, err := r.DiscoveryClient.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, r := range resources.APIResources {
		if r.Kind == kind {
			return true, nil
		}
	}
	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SelfDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	log := r.Log.WithValues("during", "selfDeploymentReconciler setup")

	// envVar set in controller-manager's Deployment YAML
	if operatorInstallNamespace, found := os.LookupEnv("INSTALL_NAMESPACE"); !found {
		err := fmt.Errorf("INSTALL_NAMESPACE must be set")
		log.Error(err, "error fetching envVar")
		return err
	} else {
		r.operatorInstallNamespace = operatorInstallNamespace
	}

	// envVar set for all operators
	if operatorNameEnvVar, found := os.LookupEnv("OPERATOR_CONDITION_NAME"); !found {
		err := fmt.Errorf("OPERATOR_CONDITION_NAME must be set")
		log.Error(err, "error fetching envVar")
		return err
	} else {
		r.operatorNameVersion = operatorNameEnvVar
	}

	config, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "error fetching controller config")
		return err
	}

	if discoveryClient, err := discovery.NewDiscoveryClientForConfig(config); err != nil {
		log.Error(err, "error setting up DiscoveryClient")
		return err
	} else {
		r.DiscoveryClient = discoveryClient
	}

	// more info: https://danielmangum.com/posts/controller-runtime-client-go-rate-limiting/
	// requeue failures & 'Requeue: true' returns starting at 30s, increasing by 30s each time to a max of 30m:
	//
	// 2021-07-26T16:24:33.263Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
	// 2021-07-26T16:25:03.266Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
	// 2021-07-26T16:26:03.269Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
	// 2021-07-26T16:28:03.272Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
	// 2021-07-26T16:32:03.272Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
	// 2021-07-26T16:40:03.272Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
	//
	customRateLimiter := workqueue.NewItemExponentialFailureRateLimiter(30*time.Second, 30*time.Minute)

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{RateLimiter: customRateLimiter}).
		For(&v1.Deployment{}).
		WithEventFilter(r.ignoreOtherDeployments()).
		Complete(r)
}

func (r *SelfDeploymentReconciler) ignoreOtherDeployments() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.evaluatePredicateObject(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func (r *SelfDeploymentReconciler) evaluatePredicateObject(obj client.Object) bool {
	lbls := obj.GetLabels()
	if obj.GetNamespace() == r.operatorInstallNamespace {
		if val, keyFound := lbls["olm.owner.kind"]; keyFound {
			if val == "ClusterServiceVersion" {
				if val, keyFound := lbls["olm.owner"]; keyFound {
					return val == r.operatorNameVersion
				}
			}
		}
	}
	return false
}
