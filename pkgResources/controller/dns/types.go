package dns

import (
	manifests "github.com/Azure/azure-provider-external-dns-e2e/pkgResources/pkgManifests"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type instance struct {
	config    *manifests.ExternalDnsConfig
	resources []client.Object
	action    action
}

type action int

const (
	deploy action = iota
	clean
)

type cleanObj struct {
	resources []client.Object
	labels    map[string]string
}
