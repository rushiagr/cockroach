// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package acceptance

import (
	gosql "database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/container"

	"github.com/cockroachdb/cockroach/acceptance/cluster"
	"github.com/cockroachdb/cockroach/acceptance/terrafarm"
	"github.com/cockroachdb/cockroach/base"
	"github.com/cockroachdb/cockroach/util/caller"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/cockroachdb/cockroach/util/randutil"
	_ "github.com/cockroachdb/pq"
)

func init() {
	flag.Parse()
}

var flagDuration = flag.Duration("d", cluster.DefaultDuration, "duration to run the test")
var flagNodes = flag.Int("nodes", 3, "number of nodes")
var flagStores = flag.Int("stores", 1, "number of stores to use for each node")
var flagRemote = flag.Bool("remote", false, "run the test using terrafarm instead of docker")
var flagCwd = flag.String("cwd", "../cloud/aws", "directory to run terraform from")
var flagKeyName = flag.String("key-name", "", "name of key for remote cluster")
var flagLogDir = flag.String("l", "", "the directory to store log files, relative to the test source")
var flagTestConfigs = flag.Bool("test-configs", false, "instead of using the passed in configuration, use the default "+
	"cluster configurations for each test. This overrides the nodes, stores and duration flags and will run the test "+
	"against a collection of pre-specified cluster configurations.")
var flagConfig = flag.String("config", "", "a json TestConfig proto, see testconfig.proto")

var flagPrivileged = flag.Bool("privileged", os.Getenv("CIRCLECI") != "true",
	"run containers in privileged mode (required for nemesis tests")
var flagTFKeepCluster = flag.Bool("tf.keep-cluster", false, "do not destroy Terraform cluster after test finishes, has precedence over tf.keep-cluster-fail")
var flagTFKeepClusterFail = flag.Bool("tf.keep-cluster-fail", false, "do not destroy Terraform cluster after test finishes only if the test has failed")

// TODO(cuongdo): These should be refactored so that they're not allocator
// test-specific when we have more than one kind of system test that uses these
// flags.
var flagATCockroachBinary = flag.String("at.cockroach-binary", "",
	"path to custom CockroachDB binary to use for allocator tests")
var flagATCockroachFlags = flag.String("at.cockroach-flags", "",
	"command-line flags to pass to cockroach for allocator tests")
var flagATCockroachEnv = flag.String("at.cockroach-env", "",
	"supervisor-style environment variables to pass to cockroach")

var testFuncRE = regexp.MustCompile("^(Test|Benchmark)")

var stopper = make(chan struct{})

func runTests(m *testing.M) {
	randutil.SeedForTests()
	go func() {
		// Shut down tests when interrupted (for example CTRL+C).
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		select {
		case <-stopper:
		default:
			// There is a very tiny race here: the cluster might be closing
			// the stopper simultaneously.
			close(stopper)
		}
	}()
	os.Exit(m.Run())
}

// prefixRE is based on a Terraform error message regarding invalid resource
// names. We perform this check to make sure that when we prepend the name
// to the Terraform-generated resource name, we have a name that meets
// Terraform's naming rules.
//
// Here's an example of the error message:
//
// * google_compute_instance.cockroach: Error creating instance: googleapi:
//   Error 400: Invalid value for field 'resource.name':
//   'Uprep-1to3-small-cockroach-1'. Must be a match of regex
//   '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)', invalid
var prefixRE = regexp.MustCompile("^(?:[a-z](?:[-a-z0-9]{0,45}[a-z0-9])?)$")

func farmer(t *testing.T, prefix string) *terrafarm.Farmer {
	if !*flagRemote {
		t.Skip("running in docker mode")
	}
	if *flagKeyName == "" {
		t.Fatal("-key-name is required") // saves a lot of trouble
	}
	logDir := *flagLogDir
	if logDir == "" {
		var err error
		logDir, err = ioutil.TempDir("", "clustertest_")
		if err != nil {
			t.Fatal(err)
		}
	}
	if !filepath.IsAbs(logDir) {
		logDir = filepath.Join(filepath.Clean(os.ExpandEnv("${PWD}")), logDir)
	}
	stores := "--store=data0"
	for j := 1; j < *flagStores; j++ {
		stores += " --store=data" + strconv.Itoa(j)
	}

	// We concatenate a random name to the prefix (for Terraform resource
	// names) to allow multiple instances of the same test to run concurrently.
	// The prefix is also used as the name of the Terraform state file.
	if prefix != "" {
		prefix += "-"
	}
	prefix += getRandomName()

	// Rudimentary collision control.
	for i := 0; ; i++ {
		newPrefix := prefix
		if i > 0 {
			newPrefix += strconv.Itoa(i)
		}
		_, err := os.Stat(filepath.Join(*flagCwd, newPrefix+".tfstate"))
		if os.IsNotExist(err) {
			prefix = newPrefix
			break
		}
	}

	if !prefixRE.MatchString(prefix) {
		t.Fatalf("generated farmer prefix '%s' must match regex %s", prefix, prefixRE)
	}
	f := &terrafarm.Farmer{
		Output:               os.Stderr,
		Cwd:                  *flagCwd,
		LogDir:               logDir,
		KeyName:              *flagKeyName,
		Stores:               stores,
		Prefix:               prefix,
		StateFile:            prefix + ".tfstate",
		AddVars:              make(map[string]string),
		KeepClusterAfterTest: *flagTFKeepCluster,
		KeepClusterAfterFail: *flagTFKeepClusterFail,
	}
	log.Infof("logging to %s", logDir)
	return f
}

// readConfigFromFlags will convert the flags to a TestConfig for the purposes
// of starting up a cluster.
func readConfigFromFlags() cluster.TestConfig {
	return cluster.TestConfig{
		Name:     fmt.Sprintf("AdHoc %dx%d", *flagNodes, *flagStores),
		Duration: *flagDuration,
		Nodes: []cluster.NodeConfig{
			{
				Count:  int32(*flagNodes),
				Stores: []cluster.StoreConfig{{Count: int32(*flagStores)}},
			},
		},
	}
}

// getConfigs returns a list of test configs based on the passed in flags.
func getConfigs(t *testing.T) []cluster.TestConfig {
	// If a config not supplied, just read the flags.
	if (flagConfig == nil || len(*flagConfig) == 0) &&
		(flagTestConfigs == nil || !*flagTestConfigs) {
		return []cluster.TestConfig{readConfigFromFlags()}
	}

	var configs []cluster.TestConfig
	if flagTestConfigs != nil && *flagTestConfigs {
		configs = append(configs, cluster.DefaultConfigs()...)
	}

	if flagConfig != nil && len(*flagConfig) > 0 {
		// Read the passed in config from the command line.
		var config cluster.TestConfig
		if err := json.Unmarshal([]byte(*flagConfig), &config); err != nil {
			t.Error(err)
		}
		configs = append(configs, config)
	}

	// Override duration in all configs if the flags are set.
	for i := 0; i < len(configs); i++ {
		// Override values.
		if flagDuration != nil && *flagDuration != cluster.DefaultDuration {
			configs[i].Duration = *flagDuration
		}
		// Set missing defaults.
		if configs[i].Duration == 0 {
			configs[i].Duration = cluster.DefaultDuration
		}
	}
	return configs
}

type configTestRunner func(*testing.T, cluster.Cluster, cluster.TestConfig)

// runTestOnConfigs retrieves the full list of test configurations and runs the
// passed in test against each on serially.
func runTestOnConfigs(t *testing.T, testFunc func(*testing.T, cluster.Cluster, cluster.TestConfig)) {
	cfgs := getConfigs(t)
	if len(cfgs) == 0 {
		t.Fatal("no config defined so most tests won't run")
	}
	for _, cfg := range cfgs {
		func() {
			cluster := StartCluster(t, cfg)
			defer cluster.AssertAndStop(t)
			testFunc(t, cluster, cfg)
		}()
	}
}

// StartCluster starts a cluster from the relevant flags. All test clusters
// should be created through this command since it sets up the logging in a
// unified way.
func StartCluster(t *testing.T, cfg cluster.TestConfig) (c cluster.Cluster) {
	var completed bool
	defer func() {
		if !completed && c != nil {
			c.AssertAndStop(t)
		}
	}()
	if !*flagRemote {
		logDir := *flagLogDir
		if logDir != "" {
			logDir = func(d string) string {
				for i := 1; i < 100; i++ {
					_, _, fun := caller.Lookup(i)
					if testFuncRE.MatchString(fun) {
						return filepath.Join(d, fun)
					}
				}
				panic("no caller matching Test(.*) in stack trace")
			}(logDir)
		}
		l := cluster.CreateLocal(cfg, logDir, *flagPrivileged, stopper)
		l.Start()
		c = l
		checkRangeReplication(t, l, 20*time.Second)
		completed = true
		return l
	}
	f := farmer(t, "")
	c = f
	if err := f.Resize(*flagNodes, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.WaitReady(5 * time.Minute); err != nil {
		_ = f.Destroy()
		t.Fatalf("cluster not ready in time: %v", err)
	}
	checkRangeReplication(t, f, 20*time.Second)
	completed = true
	return f
}

// SkipUnlessLocal calls t.Skip if not running against a local cluster.
func SkipUnlessLocal(t *testing.T) {
	if *flagRemote {
		t.Skip("skipping since not run against local cluster")
	}
}

// SkipUnlessLocal calls t.Skip if not running with the privileged flag.
func SkipUnlessPrivileged(t *testing.T) {
	if !*flagPrivileged {
		t.Skip("skipping since not run in privileged mode")
	}
}

func makePGClient(t *testing.T, dest string) *gosql.DB {
	db, err := gosql.Open("postgres", dest)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// testDockerFail ensures the specified docker cmd fails.
func testDockerFail(t *testing.T, name string, cmd []string) {
	if err := testDockerSingleNode(t, name, cmd); err == nil {
		t.Error("expected failure")
	}
}

// testDockerSuccess ensures the specified docker cmd succeeds.
func testDockerSuccess(t *testing.T, name string, cmd []string) {
	if err := testDockerSingleNode(t, name, cmd); err != nil {
		t.Error(err)
	}
}

const (
	// NB: postgresTestTag is grepped for in circle-deps.sh, so don't rename it.
	postgresTestTag = "20160705-1326"
	// Iterating against a locally built version of the docker image can be done
	// by changing postgresTestImage to the hash of the container.
	postgresTestImage = "cockroachdb/postgres-test:" + postgresTestTag
)

func testDocker(t *testing.T, num int32, name string, cmd []string) error {
	SkipUnlessLocal(t)
	cfg := cluster.TestConfig{
		Name:     name,
		Duration: *flagDuration,
		Nodes:    []cluster.NodeConfig{{Count: num, Stores: []cluster.StoreConfig{{Count: 1}}}},
	}
	l := StartCluster(t, cfg).(*cluster.LocalCluster)
	defer l.AssertAndStop(t)

	containerConfig := container.Config{
		Image: postgresTestImage,
		Env: []string{
			"PGHOST=roach0",
			fmt.Sprintf("PGPORT=%s", base.DefaultPort),
			"PGSSLCERT=/certs/node.crt",
			"PGSSLKEY=/certs/node.key",
		},
		Cmd: cmd,
	}
	hostConfig := container.HostConfig{NetworkMode: "host"}
	return l.OneShot(postgresTestImage, types.ImagePullOptions{}, containerConfig, hostConfig, "docker-"+name)
}

func testDockerSingleNode(t *testing.T, name string, cmd []string) error {
	return testDocker(t, 1, name, cmd)
}

func testDockerOneShot(t *testing.T, name string, cmd []string) error {
	return testDocker(t, 0, name, cmd)
}
