package ingress

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/azure-provider-external-dns-e2e/pkg/controller/testutils"
)

func TestNewIngressForCoverage(t *testing.T) {
	err := NewIngressControllerReconciler(&testutils.FakeManager{}, nil, "testNewIngressControllerReconciler")
	require.NoError(t, err)

	err = NewIngressClassReconciler(&testutils.FakeManager{}, nil, "testNewIngressClassReconciler")
	require.NoError(t, err)
}
