/*
Copyright 2023 Red Hat OpenShift Data Foundation.
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

	"github.com/red-hat-storage/ocs-client-operator/pkg/csi"
	"github.com/red-hat-storage/ocs-client-operator/pkg/templates"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	secv1 "github.com/openshift/api/security/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ClusterVersionReconciler reconciles a ClusterVersion object
type ClusterVersionReconciler struct {
	client.Client
	OperatorDeployment *appsv1.Deployment
	OperatorNamespace  string
	Scheme             *runtime.Scheme

	log              logr.Logger
	ctx              context.Context
	cephFSDeployment *appsv1.Deployment
	cephFSDaemonSet  *appsv1.DaemonSet
	rbdDeployment    *appsv1.Deployment
	rbdDaemonSet     *appsv1.DaemonSet
	scc              *secv1.SecurityContextConstraints
}

// SetupWithManager sets up the controller with the Manager.
func (c *ClusterVersionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configv1.ClusterVersion{}).
		Complete(c)
}

//+kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions/finalizers,verbs=update
//+kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="apps",resources=deployments/finalizers,verbs=update
//+kubebuilder:rbac:groups="apps",resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="apps",resources=daemonsets/finalizers,verbs=update
//+kubebuilder:rbac:groups="storage.k8s.io",resources=csidrivers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=create;update;delete
//+kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,verbs=get;list;watch;create;patch;update

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (c *ClusterVersionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var err error
	c.ctx = ctx
	c.log = log.FromContext(ctx, "ClusterVersion", req)
	c.log.Info("Reconciling ClusterVersion")

	instance := configv1.ClusterVersion{}
	if err = c.Client.Get(context.TODO(), req.NamespacedName, &instance); err != nil {
		return ctrl.Result{}, err
	}

	if err := csi.InitializeSidecars(instance.Status.Desired.Version); err != nil {
		c.log.Error(err, "unable to initialize sidecars")
		return ctrl.Result{}, err
	}

	c.scc = &secv1.SecurityContextConstraints{
		ObjectMeta: metav1.ObjectMeta{
			Name: csi.SCCName,
		},
	}
	err = c.createOrUpdate(c.scc, func() error {
		// TODO: this is a hack to preserve the resourceVersion of the SCC
		resourceVersion := c.scc.ResourceVersion
		csi.GetSecurityContextConstraints(c.OperatorNamespace).DeepCopyInto(c.scc)
		c.scc.ResourceVersion = resourceVersion
		return nil
	})
	if err != nil {
		c.log.Error(err, "unable to create/update SCC")
		return ctrl.Result{}, err
	}

	// create the monitor configmap for the csi drivers but never updates it.
	// This is because the monitor configurations are added to the configmap
	// when user creates storageclassclaims.
	monConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templates.MonConfigMapName,
			Namespace: c.OperatorNamespace,
		},
		Data: map[string]string{
			"config.json": "[]",
		},
	}
	if err := c.own(monConfigMap); err != nil {
		return ctrl.Result{}, err
	}
	err = c.create(monConfigMap)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		c.log.Error(err, "failed to create monitor configmap", "name", monConfigMap.Name)
		return ctrl.Result{}, err
	}

	// create the encryption configmap for the csi driver but never updates it.
	// This is because the encryption configuration are added to the configmap
	// by the users before they create the encryption storageclassclaims.
	encConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templates.EncryptionConfigMapName,
			Namespace: c.OperatorNamespace,
		},
		Data: map[string]string{
			"config.json": "[]",
		},
	}
	if err := c.own(monConfigMap); err != nil {
		return ctrl.Result{}, err
	}
	err = c.create(encConfigMap)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		c.log.Error(err, "failed to create monitor configmap", "name", encConfigMap.Name)
		return ctrl.Result{}, err
	}

	c.cephFSDeployment = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      csi.CephFSDeploymentName,
			Namespace: c.OperatorNamespace,
		},
	}
	err = c.createOrUpdate(c.cephFSDeployment, func() error {
		if err := c.own(c.cephFSDeployment); err != nil {
			return err
		}
		csi.GetCephFSDeployment(c.OperatorNamespace).DeepCopyInto(c.cephFSDeployment)
		return nil
	})
	if err != nil {
		c.log.Error(err, "failed to create/update cephfs deployment")
		return ctrl.Result{}, err
	}

	c.cephFSDaemonSet = &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      csi.CephFSDamonSetName,
			Namespace: c.OperatorNamespace,
		},
	}
	err = c.createOrUpdate(c.cephFSDaemonSet, func() error {
		if err := c.own(c.cephFSDaemonSet); err != nil {
			return err
		}
		csi.GetCephFSDaemonSet(c.OperatorNamespace).DeepCopyInto(c.cephFSDaemonSet)
		return nil
	})
	if err != nil {
		c.log.Error(err, "failed to create/update cephfs daemonset")
		return ctrl.Result{}, err
	}

	c.rbdDeployment = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      csi.RBDDeploymentName,
			Namespace: c.OperatorNamespace,
		},
	}
	err = c.createOrUpdate(c.rbdDeployment, func() error {
		if err := c.own(c.rbdDeployment); err != nil {
			return err
		}
		csi.GetRBDDeployment(c.OperatorNamespace).DeepCopyInto(c.rbdDeployment)
		return nil
	})
	if err != nil {
		c.log.Error(err, "failed to create/update rbd deployment")
		return ctrl.Result{}, err
	}

	c.rbdDaemonSet = &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      csi.RBDDaemonSetName,
			Namespace: c.OperatorNamespace,
		},
	}
	err = c.createOrUpdate(c.rbdDaemonSet, func() error {
		if err := c.own(c.rbdDaemonSet); err != nil {
			return err
		}
		csi.GetRBDDaemonSet(c.OperatorNamespace).DeepCopyInto(c.rbdDaemonSet)
		return nil
	})
	if err != nil {
		c.log.Error(err, "failed to create/update rbd daemonset")
		return ctrl.Result{}, err
	}

	// Need to handle deletion of the csiDriver object, we cannot set
	// ownerReference on it as its cluster scoped resource
	cephfsCSIDriver := templates.CephFSCSIDriver.DeepCopy()
	cephfsCSIDriver.ObjectMeta.Name = csi.GetCephFSDriverName(c.OperatorNamespace)
	err = csi.CreateCSIDriver(c.ctx, c.Client, cephfsCSIDriver)
	if err != nil {
		c.log.Error(err, "unable to create cephfs CSIDriver")
		return ctrl.Result{}, err
	}

	rbdCSIDriver := templates.RbdCSIDriver.DeepCopy()
	rbdCSIDriver.ObjectMeta.Name = csi.GetRBDDriverName(c.OperatorNamespace)
	err = csi.CreateCSIDriver(c.ctx, c.Client, rbdCSIDriver)
	if err != nil {
		c.log.Error(err, "unable to create rbd CSIDriver")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (c *ClusterVersionReconciler) createOrUpdate(obj client.Object, f controllerutil.MutateFn) error {
	result, err := controllerutil.CreateOrUpdate(c.ctx, c.Client, obj, f)
	if err != nil {
		return err
	}
	c.log.Info("successfully created or updated", "operation", result, "name", obj.GetName())
	return nil
}

func (c *ClusterVersionReconciler) own(obj client.Object) error {
	return controllerutil.SetControllerReference(c.OperatorDeployment, obj, c.Client.Scheme())
}

func (c *ClusterVersionReconciler) create(obj client.Object) error {
	return c.Client.Create(c.ctx, obj)
}
