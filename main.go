// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	pkgResources "github.com/Azure/azure-provider-external-dns-e2e/testing/e2e/pkgResources/config"
	"github.com/Azure/azure-provider-external-dns-e2e/testing/e2e/pkgResources/controller"
)

func main() {
	flag.Parse()
	rand.Seed(time.Now().Unix())

	if err := run(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func run() error {
	if err := pkgResources.Flags.Validate(); err != nil {
		return err
	}

	mgr, err := controller.NewManager(pkgResources.Flags)
	if err != nil {
		return err
	}

	return mgr.Start(ctrl.SetupSignalHandler())
}
