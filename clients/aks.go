package clients

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/go-autorest/autorest/azure"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/azure-provider-external-dns-e2e/logger"
	"github.com/Azure/azure-provider-external-dns-e2e/manifests"
)

// on push
var (
	workloadKinds   = []string{"Deployment", "StatefulSet", "DaemonSet"}
	nonZeroExitCode = errors.New("non-zero exit code")
)

// aks struct contains properties of the provisioned cluster. This struct is loaded from the infrastructure file
type aks struct {
	name, subscriptionId, resourceGroup string
	id                                  string
	dnsServiceIp                        string
	location                            string
	principalId                         string
	clientId                            string
	options                             map[string]struct{}
}

// McOpt specifies what kind of managed cluster to create
type McOpt struct {
	Name string
	fn   func(mc *armcontainerservice.ManagedCluster) error
}

// PrivateClusterOpt specifies that the cluster should be private
var PrivateClusterOpt = McOpt{
	Name: "private cluster",
	fn: func(mc *armcontainerservice.ManagedCluster) error {
		if mc.Properties == nil {
			mc.Properties = &armcontainerservice.ManagedClusterProperties{}
		}

		if mc.Properties.APIServerAccessProfile == nil {
			mc.Properties.APIServerAccessProfile = &armcontainerservice.ManagedClusterAPIServerAccessProfile{}
		}

		mc.Properties.APIServerAccessProfile.EnablePrivateCluster = to.Ptr(true)
		return nil
	},
}

// Retrieves objects from infastructure file to create aks instance
func LoadAks(id azure.Resource, dnsServiceIp, location, principalId, clientId string, options map[string]struct{}) *aks {
	return &aks{
		name:           id.ResourceName,
		subscriptionId: id.SubscriptionID,
		resourceGroup:  id.ResourceGroup,
		id:             id.String(),
		clientId:       clientId,
		dnsServiceIp:   dnsServiceIp,
		location:       location,
		principalId:    principalId,
		options:        options,
	}
}

// Creates a new public or private cluster based on mcOpt provided, saves properties in aks struct
func NewAks(ctx context.Context, subscriptionId, resourceGroup, name, location string, subnetId string, mcOpts ...McOpt) (*aks, error) {
	lgr := logger.FromContext(ctx).With("name", name, "resourceGroup", resourceGroup, "location", location)
	ctx = logger.WithContext(ctx, lgr)
	lgr.Info("starting to create aks")
	defer lgr.Info("finished creating aks")

	cred, err := GetAzCred()
	if err != nil {
		return nil, fmt.Errorf("getting az credentials: %w", err)
	}

	factory, err := armcontainerservice.NewClientFactory(subscriptionId, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating aks client factory: %w", err)
	}

	mc := armcontainerservice.ManagedCluster{
		Location: to.Ptr(location),
		Identity: &armcontainerservice.ManagedClusterIdentity{
			Type: to.Ptr(armcontainerservice.ResourceIdentityTypeSystemAssigned),
		},
		Properties: &armcontainerservice.ManagedClusterProperties{
			DNSPrefix:         to.Ptr("extdnse2e"),
			NodeResourceGroup: to.Ptr(truncate("MC_"+name, 80)),
			AgentPoolProfiles: []*armcontainerservice.ManagedClusterAgentPoolProfile{
				{
					Name:         to.Ptr("default"),
					VMSize:       to.Ptr("Standard_DS3_v2"),
					Count:        to.Ptr(int32(2)),
					Mode:         to.Ptr(armcontainerservice.AgentPoolModeSystem),
					VnetSubnetID: to.Ptr(subnetId),
				},
			},
			AddonProfiles: map[string]*armcontainerservice.ManagedClusterAddonProfile{
				"azureKeyvaultSecretsProvider": {
					Enabled: to.Ptr(true),
					Config: map[string]*string{
						"enableSecretRotation": to.Ptr("true"),
					},
				},
			},
			NetworkProfile: &armcontainerservice.NetworkProfile{
				NetworkPlugin: to.Ptr(armcontainerservice.NetworkPluginKubenet),
				IPFamilies:    []*armcontainerservice.IPFamily{to.Ptr(armcontainerservice.IPFamilyIPv4), to.Ptr(armcontainerservice.IPFamilyIPv6)},
			},
		},
	}

	options := make(map[string]struct{})
	for _, opt := range mcOpts {
		if err := opt.fn(&mc); err != nil {
			return nil, fmt.Errorf("applying cluster option: %w", err)
		}

		options[opt.Name] = struct{}{}
	}

	poll, err := factory.NewManagedClustersClient().BeginCreateOrUpdate(ctx, resourceGroup, name, mc, nil)
	if err != nil {
		return nil, fmt.Errorf("starting create cluster: %w", err)
	}

	lgr.Info(fmt.Sprintf("waiting for aks %s to be created", name))
	result, err := pollWithLog(ctx, poll, "still creating aks "+name)
	if err != nil {
		return nil, fmt.Errorf("creating cluster: %w", err)
	}

	// guard against things that should be impossible
	if result.ManagedCluster.Properties == nil {
		return nil, fmt.Errorf("managed cluster properties is nil")
	}
	if result.ManagedCluster.Properties.IdentityProfile == nil {
		return nil, fmt.Errorf("managed cluster identity profile is nil")
	}
	if result.ManagedCluster.Name == nil {
		return nil, fmt.Errorf("managed cluster name is nil")
	}
	if result.Properties.NetworkProfile.DNSServiceIP == nil {
		return nil, fmt.Errorf("dns service ip is nil")
	}

	identity, ok := result.Properties.IdentityProfile["kubeletidentity"]
	if !ok {
		return nil, fmt.Errorf("kubelet identity not found")
	}

	if identity.ObjectID == nil {
		return nil, fmt.Errorf("kubelet identity object id is nil")
	}
	if identity.ClientID == nil {
		return nil, fmt.Errorf("kubelet identity client id is nil")
	}

	return &aks{
		name:           *result.ManagedCluster.Name,
		subscriptionId: subscriptionId,
		resourceGroup:  resourceGroup,
		id:             *result.ManagedCluster.ID,
		dnsServiceIp:   *result.Properties.NetworkProfile.DNSServiceIP,
		location:       location,
		principalId:    *identity.ObjectID,
		clientId:       *identity.ClientID,
		options:        options,
	}, nil
}

// Deploys a given client.Object to cluster
func (a *aks) Deploy(ctx context.Context, objs []client.Object) error {
	lgr := logger.FromContext(ctx).With("name", a.name, "resourceGroup", a.resourceGroup)
	ctx = logger.WithContext(ctx, lgr)
	lgr.Info("starting to deploy resources")
	defer lgr.Info("finished deploying resources")

	zip, err := zipManifests(objs)
	if err != nil {
		return fmt.Errorf("zipping manifests: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(zip)
	fi, err := os.Create("./manifests.zip")
	if err != nil {
		lgr.Error("Error creating manifests.zip")
		return err
	}
	fi.Write(zip)

	if err := a.runCommand(ctx, armcontainerservice.RunCommandRequest{
		Command: to.Ptr("kubectl apply -f manifests/"),
		Context: &encoded,
	}, runCommandOpts{}); err != nil {
		return fmt.Errorf("running kubectl apply: %w", err)
	}

	if err := a.waitStable(ctx, objs); err != nil {
		return fmt.Errorf("waiting for resources to be stable: %w", err)
	}

	return nil
}

// zipManifests wraps manifests into base64 zip file.
// this is specified by the AKS ARM API.
// https://github.com/FumingZhang/azure-cli/blob/aefcf3948ed4207bfcf5d53064e5dac8ea8f19ca/src/azure-cli/azure/cli/command_modules/acs/custom.py#L2750
func zipManifests(objs []client.Object) ([]byte, error) {
	b := &bytes.Buffer{}
	zipWriter := zip.NewWriter(b)
	for i, obj := range objs {
		json, err := manifests.MarshalJson(obj)
		if err != nil {
			return nil, fmt.Errorf("marshaling json for object: %w", err)
		}

		f, err := zipWriter.Create(fmt.Sprintf("manifests/%d.json", i))
		if err != nil {
			return nil, fmt.Errorf("creating zip entry: %w", err)
		}

		if _, err := f.Write(json); err != nil {
			return nil, fmt.Errorf("writing zip entry: %w", err)
		}
	}
	zipWriter.Close()
	return b.Bytes(), nil
}

// Waits for given given pods, workloads, and jobs to complete
func (a *aks) waitStable(ctx context.Context, objs []client.Object) error {
	lgr := logger.FromContext(ctx).With("name", a.name, "resourceGroup", a.resourceGroup)
	ctx = logger.WithContext(ctx, lgr)
	lgr.Info("starting to wait for resources to be stable")
	defer lgr.Info("finished waiting for resources to be stable")

	var eg errgroup.Group
	for _, obj := range objs {
		func(obj client.Object) {
			eg.Go(func() error {
				kind := obj.GetObjectKind().GroupVersionKind().GroupKind().Kind
				ns := obj.GetNamespace()
				if ns == "" {
					ns = "default"
				}

				lgr := lgr.With("kind", kind, "name", obj.GetName(), "namespace", ns)
				lgr.Info("checking stability of " + kind + "/" + obj.GetName())

				switch {
				case slices.Contains(workloadKinds, kind):
					lgr.Info("checking rollout status")
					if err := a.runCommand(ctx, armcontainerservice.RunCommandRequest{
						Command: to.Ptr(fmt.Sprintf("kubectl rollout status %s/%s -n %s", kind, obj.GetName(), ns)),
					}, runCommandOpts{}); err != nil {
						return fmt.Errorf("waiting for %s/%s to be stable: %w", kind, obj.GetName(), err)
					}
				case kind == "Pod":
					lgr.Info("waiting for pod to be ready")
					if err := a.runCommand(ctx, armcontainerservice.RunCommandRequest{
						Command: to.Ptr(fmt.Sprintf("kubectl wait --for=condition=Ready pod/%s -n %s", obj.GetName(), ns)),
					}, runCommandOpts{}); err != nil {
						return fmt.Errorf("waiting for pod/%s to be stable: %w", obj.GetName(), err)
					}
				case kind == "Job":
					lgr.Info("waiting for job complete")

					outputFile := fmt.Sprintf("job-%s.log", obj.GetName()) // output to a file for jobs because jobs are naturally different from other deployment resources in that waiting for "stability" is waiting for them to complete
					if err := os.RemoveAll(outputFile); err != nil {       // clean out previous log file, if doesn't exist returns nil
						return fmt.Errorf("removing previous job log file: %w", err)
					}

					getLogsFn := func() error { // right now this just dumps all logs on the pod, if we eventually have more logs
						// than can be stored we will need to "stream" this by using the --since-time flag
						if err := a.runCommand(ctx, armcontainerservice.RunCommandRequest{
							Command: to.Ptr(fmt.Sprintf("kubectl logs job/%s -n %s", obj.GetName(), ns)),
						}, runCommandOpts{
							outputFile: outputFile,
						}); err != nil {
							return fmt.Errorf("waiting for job/%s to complete: %w", obj.GetName(), err)
						}

						return nil
					}

					// invoke command jobs are supposed to be short-lived, so we have to constantly poll for completion
					for {
						// check if job is complete
						if err := a.runCommand(ctx, armcontainerservice.RunCommandRequest{
							Command: to.Ptr(fmt.Sprintf("kubectl wait --for=condition=complete --timeout=5s job/%s -n %s", obj.GetName(), ns)),
						}, runCommandOpts{}); err == nil {

							break // job is complete
						} else {
							//coming here
							if !errors.Is(err, nonZeroExitCode) { // if the job is not complete, we will get a non-zero exit code

								getLogsFn()

								return fmt.Errorf("waiting for job/%s to complete: %w", obj.GetName(), err)
							}
						}

						// check if job is failed
						if err := a.runCommand(ctx, armcontainerservice.RunCommandRequest{
							Command: to.Ptr(fmt.Sprintf("kubectl wait --for=condition=failed --timeout=5s job/%s -n %s", obj.GetName(), ns)),
						}, runCommandOpts{}); err == nil {

							getLogsFn()

							return fmt.Errorf("job/%s failed", obj.GetName())
						}
					}

					if err := getLogsFn(); err != nil {
						return fmt.Errorf("getting logs for job/%s: %w", obj.GetName(), err)
					}
				}

				return nil
			})
		}(obj)
	}

	if err := eg.Wait(); err != nil {
		return fmt.Errorf("waiting for resources to be stable: %w", err)
	}

	return nil
}

// Allows you to pass in a outputfile to write the logs from runCommand()
type runCommandOpts struct {
	// outputFile is the file to write the output of the command to. Useful for saving logs from a job or something similar
	// where there's lots of logs that are extremely important and shouldn't be muddled up in the rest of the logs.
	outputFile string
}

// Runs given request on the cluster, writes logs to file if outputFile is specified via runCommandOpts
func (a *aks) runCommand(ctx context.Context, request armcontainerservice.RunCommandRequest, opt runCommandOpts) error {
	lgr := logger.FromContext(ctx).With("name", a.name, "resourceGroup", a.resourceGroup, "command", *request.Command)
	ctx = logger.WithContext(ctx, lgr)
	lgr.Info("starting to run command")
	defer lgr.Info("finished running command")

	cred, err := GetAzCred()
	if err != nil {
		return fmt.Errorf("getting az credentials: %w", err)
	}

	client, err := armcontainerservice.NewManagedClustersClient(a.subscriptionId, cred, nil)
	if err != nil {
		return fmt.Errorf("creating aks client: %w", err)
	}

	poller, err := client.BeginRunCommand(ctx, a.resourceGroup, a.name, request, nil)
	if err != nil {
		return fmt.Errorf("starting run command: %w", err)
	}

	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("running command: %w", err)
	}

	logs := ""
	if result.Properties != nil && result.Properties.Logs != nil {
		logs = *result.Properties.Logs
	}
	if opt.outputFile != "" {
		outputFile, err := os.OpenFile(opt.outputFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("creating output file %s: %w", opt.outputFile, err)
		}
		defer outputFile.Close()

		_, err = outputFile.WriteString(logs)
		if err != nil {
			return fmt.Errorf("writing output file %s: %w", opt.outputFile, err)
		}
	} else {
		lgr.Info("command output: " + logs)
	}

	if *result.Properties.ExitCode != 0 {
		lgr.Info(fmt.Sprintf("command failed with exit code %d", *result.Properties.ExitCode))
		return nonZeroExitCode
	}

	return nil
}

// Returns the provisioned aks cluster
func (a *aks) GetCluster(ctx context.Context) (*armcontainerservice.ManagedCluster, error) {
	lgr := logger.FromContext(ctx).With("name", a.name, "resourceGroup", a.resourceGroup)
	ctx = logger.WithContext(ctx, lgr)
	lgr.Info("starting to get aks")
	defer lgr.Info("finished getting aks")

	cred, err := GetAzCred()
	if err != nil {
		return nil, fmt.Errorf("getting az credentials: %w", err)
	}

	client, err := armcontainerservice.NewManagedClustersClient(a.subscriptionId, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating aks client: %w", err)
	}

	result, err := client.Get(ctx, a.resourceGroup, a.name, nil)
	if err != nil {
		return nil, fmt.Errorf("getting cluster: %w", err)
	}

	return &result.ManagedCluster, nil
}

func (a *aks) GetVnetId(ctx context.Context) (string, error) {
	lgr := logger.FromContext(ctx).With("name", a.name, "resourceGroup", a.resourceGroup)
	ctx = logger.WithContext(ctx, lgr)
	lgr.Info("starting to get vnet id for aks")
	defer lgr.Info("finished getting vnet id for aks")

	cred, err := GetAzCred()
	if err != nil {
		return "", fmt.Errorf("getting az credentials: %w", err)
	}

	client, err := armnetwork.NewVirtualNetworksClient(a.subscriptionId, cred, nil)
	if err != nil {
		return "", fmt.Errorf("creating network client: %w", err)
	}

	cluster, err := a.GetCluster(ctx)
	if err != nil {
		return "", fmt.Errorf("getting cluster: %w", err)
	}

	pager := client.NewListPager(*cluster.Properties.NodeResourceGroup, nil)
	page, err := pager.NextPage(ctx)
	if err != nil {
		return "", fmt.Errorf("listing vnet : %w", err)
	}

	vnets := page.Value
	if len(vnets) == 0 {
		return "", fmt.Errorf("no vnets found")
	}

	return *vnets[0].ID, nil
}

func (a *aks) GetId() string {
	return a.id
}

func (a *aks) GetPrincipalId() string {
	return a.principalId
}

func (a *aks) GetLocation() string {
	return a.location
}

func (a *aks) GetDnsServiceIp() string {
	return a.dnsServiceIp
}

func (a *aks) GetClientId() string {
	return a.clientId
}

func (a *aks) GetOptions() map[string]struct{} {
	return a.options
}
