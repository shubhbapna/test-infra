/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"

	"golang.org/x/crypto/ssh"

	"k8s.io/test-infra/kubetest/e2e"
	"k8s.io/test-infra/kubetest/util"
)

// kopsAWSMasterSize is the default ec2 instance type for kops on aws
const kopsAWSMasterSize = "c5.large"

const externalIPMetadataURL = "http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip"

var externalIPServiceURLs = []string{
	"https://ip.jsb.workers.dev",
	"https://v4.ifconfig.co",
}

var (

	// kops specific flags.
	kopsPath         = flag.String("kops", "", "(kops only) Path to the kops binary. kops will be downloaded from kops-base-url if not set.")
	kopsCluster      = flag.String("kops-cluster", "", "(kops only) Deprecated. Cluster name for kops; if not set defaults to --cluster.")
	kopsState        = flag.String("kops-state", "", "(kops only) s3:// path to kops state store. Must be set for the AWS provider.")
	kopsSSHUser      = flag.String("kops-ssh-user", os.Getenv("USER"), "(kops only) Username for SSH connections to nodes.")
	kopsSSHKey       = flag.String("kops-ssh-key", "", "(kops only) Path to ssh key-pair for each node (defaults '~/.ssh/kube_aws_rsa' if unset.)")
	kopsSSHPublicKey = flag.String("kops-ssh-public-key", "", "(kops only) Path to ssh public key for each node (defaults to --kops-ssh-key value with .pub suffix if unset.)")
	kopsKubeVersion  = flag.String("kops-kubernetes-version", "", "(kops only) If set, the version of Kubernetes to deploy (can be a URL to a GCS path where the release is stored) (Defaults to kops default, latest stable release.).")
	kopsZones        = flag.String("kops-zones", "", "(kops only) zones for kops deployment, comma delimited.")
	kopsNodes        = flag.Int("kops-nodes", 2, "(kops only) Number of nodes to create.")
	kopsUpTimeout    = flag.Duration("kops-up-timeout", 20*time.Minute, "(kops only) Time limit between 'kops config / kops update' and a response from the Kubernetes API.")
	kopsAdminAccess  = flag.String("kops-admin-access", "", "(kops only) If set, restrict apiserver access to this CIDR range.")
	kopsImage        = flag.String("kops-image", "", "(kops only) Image (AMI) for nodes to use. (Defaults to kops default, a Debian image with a custom kubernetes kernel.)")
	kopsArgs         = flag.String("kops-args", "", "(kops only) Additional space-separated args to pass unvalidated to 'kops create cluster', e.g. '--kops-args=\"--dns private --node-size t2.micro\"'")
	kopsPriorityPath = flag.String("kops-priority-path", "", "(kops only) Insert into PATH if set")
	kopsBaseURL      = flag.String("kops-base-url", "", "(kops only) Base URL for a prebuilt version of kops")
	kopsVersion      = flag.String("kops-version", "", "(kops only) URL to a file containing a valid kops-base-url")
	kopsDiskSize     = flag.Int("kops-disk-size", 48, "(kops only) Disk size to use for nodes and masters")
	kopsPublish      = flag.String("kops-publish", "", "(kops only) Publish kops version to the specified gs:// path on success")
	kopsMasterSize   = flag.String("kops-master-size", kopsAWSMasterSize, "(kops only) master instance type")
	kopsMasterCount  = flag.Int("kops-master-count", 1, "(kops only) Number of masters to run")
	kopsDNSProvider  = flag.String("kops-dns-provider", "", "(kops only) DNS Provider. CoreDNS or KubeDNS")
	kopsEtcdVersion  = flag.String("kops-etcd-version", "", "(kops only) Etcd Version")
	kopsNetworkMode  = flag.String("kops-network-mode", "", "(kops only) Networking mode to use. kubenet (default), classic, external, kopeio-vxlan (or kopeio), weave, flannel-vxlan (or flannel), flannel-udp, calico, canal, kube-router, romana, amazon-vpc-routed-eni, cilium.")
	kopsOverrides    = flag.String("kops-overrides", "", "(kops only) List of Kops cluster configuration overrides, comma delimited.")
	kopsFeatureFlags = flag.String("kops-feature-flags", "", "(kops only) List of Kops feature flags to enable, comma delimited.")

	kopsMultipleZones = flag.Bool("kops-multiple-zones", false, "(kops only) run tests in multiple zones")

	awsRegions = []string{
		"ap-south-1",
		"eu-west-2",
		"eu-west-1",
		"ap-northeast-2",
		"ap-northeast-1",
		"sa-east-1",
		"ca-central-1",
		// not supporting Singapore since they do not seem to have capacity for c4.large
		//"ap-southeast-1",
		"ap-southeast-2",
		"eu-central-1",
		"us-east-1",
		"us-east-2",
		"us-west-1",
		"us-west-2",
		// not supporting Paris yet as AWS does not have all instance types available
		//"eu-west-3",
	}
)

type kops struct {
	path        string
	kubeVersion string
	zones       []string
	nodes       int
	adminAccess string
	cluster     string
	image       string
	args        string
	kubecfg     string
	diskSize    int

	// sshUser is the username to use when SSHing to nodes (for example for log capture)
	sshUser string
	// sshPublicKey is the path to the SSH public key matching sshPrivateKey
	sshPublicKey string
	// sshPrivateKey is the path to the SSH private key matching sshPublicKey
	sshPrivateKey string

	// GCP project we should use
	gcpProject string

	// Cloud provider in use (gce, aws)
	provider string

	// kopsVersion is the version of kops we are running (used for publishing)
	kopsVersion string

	// kopsPublish is the path where we will publish kopsVersion, after a successful test
	kopsPublish string

	// masterCount denotes how many masters to start
	masterCount int

	// dnsProvider is the DNS Provider the cluster will use (CoreDNS or KubeDNS)
	dnsProvider string

	// etcdVersion is the etcd version to run
	etcdVersion string

	// masterSize is the EC2 instance type for the master
	masterSize string

	// networkMode is the networking mode to use for the cluster (e.g kubenet)
	networkMode string

	// overrides is a list of cluster configuration overrides, comma delimited
	overrides string

	// featureFlags is a list of feature flags to enable, comma delimited
	featureFlags string
}

var _ deployer = kops{}

func migrateKopsEnv() error {
	return util.MigrateOptions([]util.MigratedOption{
		{
			Env:      "KOPS_STATE_STORE",
			Option:   kopsState,
			Name:     "--kops-state",
			SkipPush: true,
		},
		{
			Env:      "AWS_SSH_KEY",
			Option:   kopsSSHKey,
			Name:     "--kops-ssh-key",
			SkipPush: true,
		},
		{
			Env:      "PRIORITY_PATH",
			Option:   kopsPriorityPath,
			Name:     "--kops-priority-path",
			SkipPush: true,
		},
	})
}

func newKops(provider, gcpProject, cluster string) (*kops, error) {
	tmpdir, err := os.MkdirTemp("", "kops")
	if err != nil {
		return nil, err
	}

	if err := migrateKopsEnv(); err != nil {
		return nil, err
	}

	if *kopsCluster != "" {
		cluster = *kopsCluster
	}
	if cluster == "" {
		return nil, fmt.Errorf("--cluster or --kops-cluster must be set to a valid cluster name for kops deployment")
	}
	if *kopsState == "" && provider != "gce" {
		return nil, fmt.Errorf("--kops-state must be set to a valid S3 path for kops deployments on AWS")
	} else if provider == "gce" {
		kopsState, err = setupGCEStateStore(gcpProject)
		if err != nil {
			return nil, err
		}
	}

	if *kopsPriorityPath != "" {
		if err := util.InsertPath(*kopsPriorityPath); err != nil {
			return nil, err
		}
	}

	// TODO(fejta): consider explicitly passing these env items where needed.
	sshKey := *kopsSSHKey
	if sshKey == "" {
		usr, err := user.Current()
		if err != nil {
			return nil, err
		}
		sshKey = filepath.Join(usr.HomeDir, ".ssh/kube_aws_rsa")
	}
	if err := os.Setenv("KOPS_STATE_STORE", *kopsState); err != nil {
		return nil, err
	}
	sshPublicKey := *kopsSSHPublicKey
	if sshPublicKey == "" {
		sshPublicKey = sshKey + ".pub"
	}

	sshUser := *kopsSSHUser
	if sshUser != "" {
		if err := os.Setenv("KUBE_SSH_USER", sshUser); err != nil {
			return nil, err
		}
	}

	// Repoint KUBECONFIG to an isolated kubeconfig in our temp directory
	kubecfg := filepath.Join(tmpdir, "kubeconfig")
	f, err := os.Create(kubecfg)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return nil, err
	}
	if err := os.Setenv("KUBECONFIG", kubecfg); err != nil {
		return nil, err
	}

	// Set KUBERNETES_CONFORMANCE_TEST so the auth info is picked up
	// from kubectl instead of bash inference.
	if err := os.Setenv("KUBERNETES_CONFORMANCE_TEST", "yes"); err != nil {
		return nil, err
	}
	// Set KUBERNETES_CONFORMANCE_PROVIDER to override the
	// cloudprovider for KUBERNETES_CONFORMANCE_TEST.
	// This value is set by the provider flag that is passed into kubetest.
	// HACK: until we merge #7408, there's a bug in the ginkgo-e2e.sh script we have to work around
	// TODO(justinsb): remove this hack once #7408 merges
	// if err := os.Setenv("KUBERNETES_CONFORMANCE_PROVIDER", provider); err != nil {
	if err := os.Setenv("KUBERNETES_CONFORMANCE_PROVIDER", "aws"); err != nil {
		return nil, err
	}
	// AWS_SSH_KEY is required by the AWS e2e tests.
	if err := os.Setenv("AWS_SSH_KEY", sshKey); err != nil {
		return nil, err
	}

	// zones are required by the kops e2e tests.
	var zones []string

	// if zones is set to zero and gcp project is not set then pick random aws zone
	if *kopsZones == "" && provider == "aws" {
		zones, err = getRandomAWSZones(*kopsMasterCount, *kopsMultipleZones)
		if err != nil {
			return nil, err
		}
	} else {
		zones = strings.Split(*kopsZones, ",")
	}

	// set ZONES for e2e.go
	if err := os.Setenv("ZONE", zones[0]); err != nil {
		return nil, err
	}

	if len(zones) == 0 {
		return nil, errors.New("no zones found")
	} else if zones[0] == "" {
		return nil, errors.New("zone cannot be a empty string")
	}

	log.Printf("executing kops with zones: %q", zones)

	// Set kops-base-url from kops-version
	if *kopsVersion != "" {
		if *kopsBaseURL != "" {
			return nil, fmt.Errorf("cannot set --kops-version and --kops-base-url")
		}

		var b bytes.Buffer
		if err := httpRead(*kopsVersion, &b); err != nil {
			return nil, err
		}
		latest := strings.TrimSpace(b.String())

		log.Printf("Got latest kops version from %v: %v", *kopsVersion, latest)
		if latest == "" {
			return nil, fmt.Errorf("version URL %v was empty", *kopsVersion)
		}
		*kopsBaseURL = latest
	}

	// kops looks at KOPS_BASE_URL env var, so export it here
	if *kopsBaseURL != "" {
		if err := os.Setenv("KOPS_BASE_URL", *kopsBaseURL); err != nil {
			return nil, err
		}
	}

	// Download kops from kopsBaseURL if kopsPath is not set
	if *kopsPath == "" {
		if *kopsBaseURL == "" {
			return nil, errors.New("--kops or --kops-base-url must be set")
		}

		kopsBinURL := *kopsBaseURL + "/linux/amd64/kops"
		log.Printf("Download kops binary from %s", kopsBinURL)
		kopsBin := filepath.Join(tmpdir, "kops")
		f, err := os.Create(kopsBin)
		if err != nil {
			return nil, fmt.Errorf("error creating file %q: %w", kopsBin, err)
		}
		defer f.Close()
		if err := httpRead(kopsBinURL, f); err != nil {
			return nil, err
		}
		if err := util.EnsureExecutable(kopsBin); err != nil {
			return nil, err
		}
		*kopsPath = kopsBin
	}

	return &kops{
		path:          *kopsPath,
		kubeVersion:   *kopsKubeVersion,
		sshPrivateKey: sshKey,
		sshPublicKey:  sshPublicKey,
		sshUser:       sshUser,
		zones:         zones,
		nodes:         *kopsNodes,
		adminAccess:   *kopsAdminAccess,
		cluster:       cluster,
		image:         *kopsImage,
		args:          *kopsArgs,
		kubecfg:       kubecfg,
		provider:      provider,
		gcpProject:    gcpProject,
		diskSize:      *kopsDiskSize,
		kopsVersion:   *kopsBaseURL,
		kopsPublish:   *kopsPublish,
		masterCount:   *kopsMasterCount,
		dnsProvider:   *kopsDNSProvider,
		etcdVersion:   *kopsEtcdVersion,
		masterSize:    *kopsMasterSize,
		networkMode:   *kopsNetworkMode,
		overrides:     *kopsOverrides,
		featureFlags:  *kopsFeatureFlags,
	}, nil
}

func (k kops) isGoogleCloud() bool {
	return k.provider == "gce"
}

func (k kops) Up() error {
	// If we downloaded kubernetes, pass that version to kops
	if k.kubeVersion == "" {
		// TODO(justinsb): figure out a refactor that allows us to get this from acquireKubernetes cleanly
		kubeReleaseURL := os.Getenv("KUBERNETES_RELEASE_URL")
		kubeRelease := os.Getenv("KUBERNETES_RELEASE")
		if kubeReleaseURL != "" && kubeRelease != "" {
			if !strings.HasSuffix(kubeReleaseURL, "/") {
				kubeReleaseURL += "/"
			}
			k.kubeVersion = kubeReleaseURL + kubeRelease
		}
	}

	createArgs := []string{
		"create", "cluster",
		"--name", k.cluster,
		"--ssh-public-key", k.sshPublicKey,
		"--node-count", strconv.Itoa(k.nodes),
		"--node-volume-size", strconv.Itoa(k.diskSize),
		"--master-volume-size", strconv.Itoa(k.diskSize),
		"--master-count", strconv.Itoa(k.masterCount),
		"--zones", strings.Join(k.zones, ","),
	}

	var featureFlags []string
	if k.featureFlags != "" {
		featureFlags = append(featureFlags, k.featureFlags)
	}
	var overrides []string
	if k.overrides != "" {
		overrides = append(overrides, k.overrides)
	}

	// We are defaulting the master size to c5.large on AWS because it's cheapest non-throttled instance type.
	// When we are using GCE, then we need to handle the flag differently.
	// If we are not using gce then add the masters size flag, or if we are using gce, and the
	// master size is not set to the aws default, then add the master size flag.
	if !k.isGoogleCloud() || (k.isGoogleCloud() && k.masterSize != kopsAWSMasterSize) {
		createArgs = append(createArgs, "--master-size", k.masterSize)
	}

	if k.kubeVersion != "" {
		createArgs = append(createArgs, "--kubernetes-version", k.kubeVersion)
	}
	if k.adminAccess == "" {
		externalIPRange, err := getExternalIPRange()
		if err != nil {
			return fmt.Errorf("external IP cannot be retrieved: %w", err)
		}

		log.Printf("Using external IP for admin access: %v", externalIPRange)
		k.adminAccess = externalIPRange
	}
	createArgs = append(createArgs, "--admin-access", k.adminAccess)

	// Since https://github.com/kubernetes/kubernetes/pull/80655 conformance now require node ports to be open to all nodes
	overrides = append(overrides, "cluster.spec.nodePortAccess=0.0.0.0/0")

	if k.image != "" {
		createArgs = append(createArgs, "--image", k.image)
	}
	if k.gcpProject != "" {
		createArgs = append(createArgs, "--project", k.gcpProject)
	}
	if k.isGoogleCloud() {
		featureFlags = append(featureFlags, "AlphaAllowGCE")
		createArgs = append(createArgs, "--cloud", "gce")
	} else {
		// append cloud type to allow for use of new regions without updates
		createArgs = append(createArgs, "--cloud", "aws")
	}
	if k.networkMode != "" {
		createArgs = append(createArgs, "--networking", k.networkMode)
	}
	if k.args != "" {
		createArgs = append(createArgs, strings.Split(k.args, " ")...)
	}
	if k.dnsProvider != "" {
		overrides = append(overrides, "spec.kubeDNS.provider="+k.dnsProvider)
	}
	if k.etcdVersion != "" {
		overrides = append(overrides, "cluster.spec.etcdClusters[*].version="+k.etcdVersion)
	}
	if len(overrides) != 0 {
		featureFlags = append(featureFlags, "SpecOverrideFlag")
		createArgs = append(createArgs, "--override", strings.Join(overrides, ","))
	}
	if len(featureFlags) != 0 {
		os.Setenv("KOPS_FEATURE_FLAGS", strings.Join(featureFlags, ","))
	}

	createArgs = append(createArgs, "--yes")

	if err := control.FinishRunning(exec.Command(k.path, createArgs...)); err != nil {
		return fmt.Errorf("kops create cluster failed: %w", err)
	}

	// TODO: Once this gets support for N checks in a row, it can replace the above node readiness check
	if err := control.FinishRunning(exec.Command(k.path, "validate", "cluster", k.cluster, "--wait", "15m")); err != nil {
		return fmt.Errorf("kops validate cluster failed: %w", err)
	}

	// We require repeated successes, so we know that the cluster is stable
	// (e.g. in HA scenarios, or where we're using multiple DNS servers)
	// We use a relatively high number as DNS can take a while to
	// propagate across multiple servers / caches
	requiredConsecutiveSuccesses := 10

	// Wait for nodes to become ready
	if err := waitForReadyNodes(k.nodes+1, *kopsUpTimeout, requiredConsecutiveSuccesses); err != nil {
		return fmt.Errorf("kops nodes not ready: %w", err)
	}

	return nil
}

// getExternalIPRange returns the external IP range where the test job
// is running, e.g. 8.8.8.8/32, useful for restricting access to the
// apiserver and any other exposed endpoints.
func getExternalIPRange() (string, error) {
	var b bytes.Buffer

	err := httpReadWithHeaders(externalIPMetadataURL, map[string]string{"Metadata-Flavor": "Google"}, &b)
	if err != nil {
		// This often fails due to workload identity
		log.Printf("failed to get external ip from metadata service: %v", err)
	} else if ip := net.ParseIP(strings.TrimSpace(b.String())); ip != nil {
		return ip.String() + "/32", nil
	} else {
		log.Printf("metadata service returned invalid ip %q", b.String())
	}

	for attempt := 0; attempt < 5; attempt++ {
		for _, u := range externalIPServiceURLs {
			b.Reset()
			err = httpRead(u, &b)
			if err != nil {
				// The external service may well be down
				log.Printf("failed to get external ip from %s: %v", u, err)
			} else if ip := net.ParseIP(strings.TrimSpace(b.String())); ip != nil {
				return ip.String() + "/32", nil
			} else {
				log.Printf("service %s returned invalid ip %q", u, b.String())
			}
		}

		time.Sleep(2 * time.Second)
	}

	return "", fmt.Errorf("external IP cannot be retrieved")
}

func (k kops) IsUp() error {
	return isUp(k)
}

func (k kops) DumpClusterLogs(localPath, gcsPath string) error {
	privateKeyPath := k.sshPrivateKey
	if strings.HasPrefix(privateKeyPath, "~/") {
		privateKeyPath = filepath.Join(os.Getenv("HOME"), privateKeyPath[2:])
	}
	key, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return fmt.Errorf("error reading private key %q: %w", k.sshPrivateKey, err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("error parsing private key %q: %w", k.sshPrivateKey, err)
	}

	sshConfig := &ssh.ClientConfig{
		User: k.sshUser,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	sshClientFactory := &sshClientFactoryImplementation{
		sshConfig: sshConfig,
	}
	logDumper, err := newLogDumper(sshClientFactory, localPath)
	if err != nil {
		return err
	}

	// Capture sysctl settings
	logDumper.DumpSysctls = true

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	finished := make(chan error)
	go func() {
		finished <- k.dumpAllNodes(ctx, logDumper)
	}()

	logDumper.dumpPods(ctx, "kube-system", nil)

	for {
		select {
		case <-interrupt.C:
			cancel()
		case err := <-finished:
			return err
		}
	}
}

// dumpAllNodes connects to every node and dumps the logs
func (k *kops) dumpAllNodes(ctx context.Context, d *logDumper) error {
	// Make sure kubeconfig is set, in particular before calling DumpAllNodes, which calls kubectlGetNodes
	if err := k.TestSetup(); err != nil {
		return fmt.Errorf("error setting up kubeconfig: %w", err)
	}

	var additionalIPs []string
	dump, err := k.runKopsDump()
	if err != nil {
		log.Printf("unable to get cluster status from kops: %v", err)
	} else {
		for _, instance := range dump.Instances {
			name := instance.Name

			if len(instance.PublicAddresses) == 0 {
				log.Printf("ignoring instance in kops status with no public address: %v", name)
				continue
			}

			additionalIPs = append(additionalIPs, instance.PublicAddresses[0])
		}
	}

	if err := d.DumpAllNodes(ctx, additionalIPs); err != nil {
		return err
	}

	return nil
}

func (k kops) TestSetup() error {
	info, err := os.Stat(k.kubecfg)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("kubeconfig file %s not found", k.kubecfg)
		} else {
			return err
		}
	} else if info.Size() > 0 {
		// Assume that if we already have it, it's good.
		return nil
	}

	if err := control.FinishRunning(exec.Command(k.path, "export", "kubecfg", k.cluster)); err != nil {
		return fmt.Errorf("failure from 'kops export kubecfg %s': %w", k.cluster, err)
	}

	// Double-check that the file was exported
	info, err = os.Stat(k.kubecfg)
	if err != nil {
		return fmt.Errorf("kubeconfig file %s was not exported", k.kubecfg)
	}
	if info.Size() == 0 {
		return fmt.Errorf("exported kubeconfig file %s was empty", k.kubecfg)
	}

	return nil
}

// BuildTester returns a standard ginkgo-script tester, except for GCE where we build an e2e.Tester
func (k kops) BuildTester(o *e2e.BuildTesterOptions) (e2e.Tester, error) {
	kubecfg, err := parseKubeconfig(k.kubecfg)
	if err != nil {
		return nil, fmt.Errorf("error parsing kubeconfig %q: %w", k.kubecfg, err)
	}

	log.Printf("running ginkgo tests directly")

	t := e2e.NewGinkgoTester(o)
	t.KubeRoot = "."

	t.Kubeconfig = k.kubecfg
	t.Provider = k.provider

	t.ClusterID = k.cluster

	if len(kubecfg.Clusters) > 0 {
		t.KubeMasterURL = kubecfg.Clusters[0].Cluster.Server
	}

	if k.provider == "gce" {
		t.GCEProject = k.gcpProject
		if len(k.zones) > 0 {
			zone := k.zones[0]
			t.GCEZone = zone

			// us-central1-a => us-central1
			lastDash := strings.LastIndex(zone, "-")
			if lastDash == -1 {
				return nil, fmt.Errorf("unexpected format for GCE zone: %q", zone)
			}
			t.GCERegion = zone[0:lastDash]
		}
	} else if k.provider == "aws" {
		if len(k.zones) > 0 {
			zone := k.zones[0]
			// These GCE fields are actually provider-agnostic
			t.GCEZone = zone

			if zone == "" {
				return nil, errors.New("zone cannot be a empty string")
			}

			// us-east-1a => us-east-1
			t.GCERegion = zone[0 : len(zone)-1]
		}
	}

	return t, nil
}

func (k kops) Down() error {
	// We do a "kops get" first so the exit status of "kops delete" is
	// more sensical in the case of a non-existent cluster. ("kops
	// delete" will exit with status 1 on a non-existent cluster)
	err := control.FinishRunning(exec.Command(k.path, "get", "clusters", k.cluster))
	if err != nil {
		// This is expected if the cluster doesn't exist.
		return nil
	}
	control.FinishRunning(exec.Command(k.path, "delete", "cluster", k.cluster, "--yes"))
	if kopsState != nil && k.isGoogleCloud() {
		ctx := context.Background()
		client, err := storage.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("error building storage API client: %w", err)
		}
		bkt := client.Bucket(*kopsState)
		if err := bkt.Delete(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (k kops) GetClusterCreated(gcpProject string) (time.Time, error) {
	return time.Time{}, errors.New("not implemented")
}

// kopsDump is the format of data as dumped by `kops toolbox dump -ojson`
type kopsDump struct {
	Instances []*kopsDumpInstance `json:"instances"`
}

// String implements fmt.Stringer
func (o *kopsDump) String() string {
	return util.JSONForDebug(o)
}

// kopsDumpInstance is the format of an instance (machine) in a kops dump
type kopsDumpInstance struct {
	Name            string   `json:"name"`
	PublicAddresses []string `json:"publicAddresses"`
}

// String implements fmt.Stringer
func (o *kopsDumpInstance) String() string {
	return util.JSONForDebug(o)
}

// runKopsDump runs a kops toolbox dump to dump the status of the cluster
func (k *kops) runKopsDump() (*kopsDump, error) {
	o, err := control.Output(exec.Command(k.path, "toolbox", "dump", "--name", k.cluster, "-ojson"))
	if err != nil {
		log.Printf("error running kops toolbox dump: %s\n%s", wrapError(err).Error(), string(o))
		return nil, err
	}

	dump := &kopsDump{}
	if err := json.Unmarshal(o, dump); err != nil {
		return nil, fmt.Errorf("error parsing kops toolbox dump output: %w", err)
	}

	return dump, nil
}

// kops deployer implements publisher
var _ publisher = &kops{}

// kops deployer implements e2e.TestBuilder
var _ e2e.TestBuilder = &kops{}

// Publish will publish a success file, it is called if the tests were successful
func (k kops) Publish() error {
	if k.kopsPublish == "" {
		// No publish destination set
		return nil
	}

	if k.kopsVersion == "" {
		return errors.New("kops-version not set; cannot publish")
	}

	return control.XMLWrap(&suite, "Publish kops version", func() error {
		log.Printf("Set %s version to %s", k.kopsPublish, k.kopsVersion)
		return gcsWrite(k.kopsPublish, []byte(k.kopsVersion))
	})
}

func (k kops) KubectlCommand() (*exec.Cmd, error) { return nil, nil }

// getRandomAWSZones looks up all regions, and the availability zones for those regions.  A random
// region is then chosen and the AZ's for that region is returned. At least masterCount zones will be
// returned, all in the same region.
func getRandomAWSZones(masterCount int, multipleZones bool) ([]string, error) {

	// TODO(chrislovecnm): get the number of ec2 instances in the region and ensure that there are not too many running
	for _, i := range rand.Perm(len(awsRegions)) {
		ec2Session, err := getAWSEC2Session(awsRegions[i])
		if err != nil {
			return nil, err
		}

		// az for a region. AWS Go API does not allow us to make a single call
		zoneResults, err := ec2Session.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{})
		if err != nil {
			return nil, fmt.Errorf("unable to call aws api DescribeAvailabilityZones for %q: %w", awsRegions[i], err)
		}

		var selectedZones []string
		if len(zoneResults.AvailabilityZones) >= masterCount && multipleZones {
			for _, z := range zoneResults.AvailabilityZones {
				selectedZones = append(selectedZones, *z.ZoneName)
			}

			log.Printf("Launching cluster in region: %q", awsRegions[i])
			return selectedZones, nil
		} else if !multipleZones {
			z := zoneResults.AvailabilityZones[rand.Intn(len(zoneResults.AvailabilityZones))]
			selectedZones = append(selectedZones, *z.ZoneName)
			log.Printf("Launching cluster in region: %q", awsRegions[i])
			return selectedZones, nil
		}
	}

	return nil, fmt.Errorf("unable to find region with %d zones", masterCount)
}

// getAWSEC2Session creates an returns a EC2 API session.
func getAWSEC2Session(region string) (*ec2.EC2, error) {
	config := aws.NewConfig().WithRegion(region)

	// This avoids a confusing error message when we fail to get credentials
	config = config.WithCredentialsChainVerboseErrors(true)

	s, err := session.NewSession(config)
	if err != nil {
		return nil, fmt.Errorf("unable to build aws API session with region: %q: %w", region, err)
	}

	return ec2.New(s, config), nil
}

// kubeconfig is a simplified version of the kubernetes Config type
type kubeconfig struct {
	Clusters []struct {
		Cluster struct {
			Server string `json:"server"`
		} `json:"cluster"`
	} `json:"clusters"`
}

// parseKubeconfig uses kubectl to extract the current kubeconfig configuration
func parseKubeconfig(kubeconfigPath string) (*kubeconfig, error) {
	cmd := "kubectl"

	o, err := control.Output(exec.Command(cmd, "config", "view", "--minify", "-ojson", "--kubeconfig", kubeconfigPath))
	if err != nil {
		log.Printf("kubectl config view failed: %s\n%s", wrapError(err).Error(), string(o))
		return nil, err
	}

	cfg := &kubeconfig{}
	if err := json.Unmarshal(o, cfg); err != nil {
		return nil, fmt.Errorf("error parsing kubectl config view output: %w", err)
	}

	return cfg, nil
}

// setupGCEStateStore is used to create a 1-off state bucket in the active GCP project
func setupGCEStateStore(projectId string) (*string, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("error building storage API client: %w", err)
	}
	name := gceBucketName(projectId)
	bkt := client.Bucket(name)
	if err := bkt.Create(ctx, projectId, nil); err != nil {
		return nil, err
	}
	log.Printf("Created new GCS bucket for state store: %s\n.", name)
	store := fmt.Sprintf("gs://%s", name)
	return &store, nil
}

// gceBucketName generates a name for GCE state store bucket
func gceBucketName(projectId string) string {
	b := make([]byte, 2)
	rand.Read(b)
	s := hex.EncodeToString(b)
	return strings.Join([]string{projectId, "state", s}, "-")
}
