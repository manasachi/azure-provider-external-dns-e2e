// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingress

import (
	"time"

	"github.com/Azure/azure-provider-external-dns-e2e/testing/e2e/pkgResources/controller/common"
	"github.com/Azure/azure-provider-external-dns-e2e/testing/e2e/pkgResources/controller/controllername"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const reconcileInterval = time.Minute * 3

// NewIngressControllerReconciler creates a reconciler that manages ingress controller resources
func NewIngressControllerReconciler(manager ctrl.Manager, resources []client.Object, name string) error {
	return common.NewResourceReconciler(manager, controllername.New(name, "ingress", "controller", "reconciler"), resources, reconcileInterval)
}
