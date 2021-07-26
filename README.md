## What's this project?

An example showcase demonstrating how DBaaS Provider operators that wish to partner with the Red Hat DBaaS Operator
 can create a cluster-scoped resource at startup with ownership defined that ensures the resource is garbage 
collected when the operator is uninstalled via OLM. 

## Notes
- For partners doing actual implementation, the resource created 
within, the ClusterScopedResource, would be a DBaaSProvider resource instead.
- Both resource creation and removal is automated, meaning that they will occur just by virtue of 
  installing/uninstalling this operator via OLM
- Code uses 2 environment variables:
  - **OPERATOR_CONDITION_NAME**: specified the Name/Version of the operator 
     - automatically added to all operators installed via OLM
  - **INSTALL_NAMESPACE**: specifies what namespace the operator is running in
    - NOT to be confused with a watch namespace since operator is cluster-scoped
    - added via the Deployment YAML within the manager.yaml template file:
    ```yaml
    env:
      - name: INSTALL_NAMESPACE
        valueFrom:
          fieldRef:
            fieldPath: metadata.namespace
    ```

## Self-Deployment Controller Config & Registration

### Registration
- At start-up, register a new controller with manager that will reconcile this operator's deployment:
```go
if err = (&controllers.SelfDeploymentReconciler{
    Client: mgr.GetClient(),
    Log:    ctrl.Log.WithName("controllers").WithName("SelfDeployment"),
    Scheme: mgr.GetScheme(),
}).SetupWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create controller", "controller", "SelfDeployment")
    os.Exit(1)
}
```

### Environment Variables
- within the SetupWithManager func, grab the 2 EnvVars and set them on the reconciler:
```go
func (r *SelfDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {

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

	...
}
```

### CRD Discovery
- also within SetupWithManager, initiate the Discovery client used to check for CRD presence:
```go
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

```  

### Retry Rate Limiting

- next, we want to replace the default requeue rate limiter with one more appropriate for retrying the test for CRD
presence. In this example, the retries will start at 30s apart, increase by double each attempt 
  (30s, 1m, 2m, 4m, etc) and max out at 30m between retries.
```go
	customRateLimiter := workqueue.NewItemExponentialFailureRateLimiter(30*time.Second, 30*time.Minute)
	
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{RateLimiter: customRateLimiter}).
		...

```  
- example output reflecting the custom rate limiter exponential backoff beginning with 30s:
```
// 2021-07-26T16:24:33.263Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
// 2021-07-26T16:25:03.266Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
// 2021-07-26T16:26:03.269Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
// 2021-07-26T16:28:03.272Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
// 2021-07-26T16:32:03.272Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
// 2021-07-26T16:40:03.272Z	INFO	controllers.SelfDeployment	CRD not found, requeueing with rate limiter
```

### Predicate Filtering

- Lastly, the reconciler, by default, would pick up every deployment on the cluster, so use Event predicates to ensure 
  that we ONLY reconcile when a 'create' event is issued for the deployment of this operator:
```go
	return ctrl.NewControllerManagedBy(mgr).
		...
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
```

## Creating the Auto-Cleaned Cluster-Scoped Resource

- upon entering reconcile for the operator's deployment 'create' event, we start by checking that the target CRD
is registered with the cluster and, if not found, requeue using the customer rate limited described above. Note
  that with DBaaS Provider operators, you'd be checking for `DBaaSProvider` from `dbaas.redhat.com/v1alpha1`:
```go
found, err := r.checkCrdInstalled("resource.acme.com/v1alpha1", "ClusterScopedResource")
if err != nil {
    log.Error(err, "error discovering GVK")
    return ctrl.Result{}, err
}
if !found {
    log.Info("CRD not found, requeueing with rate limiter")
    // returning with 'Requeue: true' will invoke our custom rate limiter seen in SetupWithManager
    return ctrl.Result{Requeue: true}, nil
} else {
	...
}

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
```

- if the CRD is present, we can then create the target custom resource if not already present. Start by fetching 
  the associated ClusterRole so that we know who will 'own' our new cluster-scope resource:
```go
opts := &client.ListOptions{
    LabelSelector: labels.SelectorFromSet(map[string]string{
        "olm.owner":      r.operatorNameVersion,
        "olm.owner.kind": "ClusterServiceVersion",
    }),
}
clusterRoleList := &rbac.ClusterRoleList{}
if err := apiReader.List(context.Background(), clusterRoleList, opts); err != nil {
    r.Log.Error(err, "unable to list ClusterRoles to seek potential operand owners")
    return err
}
```
- create the new cluster-scope resource with owner defined to ensure cascade delete cleans the resource up (this would 
  be a DBaaSProvider resource in actual implementation):
```go
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
```
- on uninstall of the operator, OLM cleans up the ClusterRole & our resource is garbage-collected given the
  OwnerReference we defined. 

## RBAC:

This project also shows how to indicate a resource is cluster-scoped (via kubebuilder label):
```go
//+kubebuilder:resource:scope=Cluster

type ClusterScopedResource struct {
	...
}
```
I also added a ClusterRole/RoleBinding to demonstrate how to work with cluster-scoped resources that aren't 'owned' by
the operator:
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cluster-deployment-accessor-role
rules:
  - apiGroups:
      - apps
    resources:
      - deployments
    verbs:
      - create
      - delete
      - get
      - list
      - patch
      - update
      - watch
```
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cluster-deployment-accessors
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-deployment-accessor-role
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: system:authenticated

```