package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"io/ioutil"
	"bytes"
	"errors"
	"encoding/json"
	"net"
	"net/http"
	"github.com/couchbaselabs/sdk-doctor/helpers"
	"github.com/couchbaselabs/sdk-doctor/connstr"
	"time"
)

// diagnoseCmd represents the diagnose command
var diagnoseCmd = &cobra.Command{
	Use:   "diagnose [connection_string]",
	Short: "Diagnose checks for problems with your configuration",
	Long: `Diagnose runs various tests against you're network and cluster
to identify any flaws in your configuration that would cause failures
in development or production environments.`,
	RunE: RunDiagnose,
}

var (
	bucketPasswordArg string
)

func init() {
	RootCmd.AddCommand(diagnoseCmd)

	diagnoseCmd.PersistentFlags().StringVarP(&bucketPasswordArg, "bucket-password", "p", "", "bucket password")
}

var gLog helpers.Logger
var gHttpClient http.Client

func RunDiagnose(cmd *cobra.Command, args []string) error {
	fmt.Printf("|====================================================================|\n")
	fmt.Printf("|          ___ ___  _  __   ___   ___   ___ _____ ___  ___           |\n")
	fmt.Printf("|         / __|   \\| |/ /__|   \\ / _ \\ / __|_   _/ _ \\| _ \\          |\n")
	fmt.Printf("|         \\__ \\ |) | ' <___| |) | (_) | (__  | || (_) |   /          |\n")
	fmt.Printf("|         |___/___/|_|\\_\\  |___/ \\___/ \\___| |_| \\___/|_|_\\          |\n")
	fmt.Printf("|                                                                    |\n")
	fmt.Printf("|====================================================================|\n")
	fmt.Printf("\n")

	fmt.Printf(
		"Note: Diagnostics can only provide accurate results when you're cluster\n" +
		" is in a stable state.  Active rebalancing and other cluster configuration\n" +
		" changes can cause the output of the doctor to be inconsistent or in the\n" +
		" worst cases, completely incorrect.\n")
	fmt.Printf("\n")

	if len(args) < 1 {
		return errors.New("You must specify a connection string for your cluster")
	}

	Diagnose(args[0], bucketPasswordArg)

	gLog.Log("Diagnostics completed")
	gLog.NewLine()

	gLog.PrintSummary()

	return nil
}


type ClusterConfigNode struct {
	OptNode string `json:"optNode"`
	ThisNode bool `json:"thisNode"`
	CouchApiBase string `json:"couchApiBase"`
	CouchApiBaseHTTPS string `json:"couchApiBaseHTTPS"`
	Status string `json:"status"`
	Hostname string `json:"hostname"`
	Version string `json:"version"`
	Os string `json:"os"`
	Ports map[string]int `json:"Ports"`
	Services []string `json:"services"`
}

type ClusterConfig struct {
	Nodes []ClusterConfigNode `json:"nodes"`
	Buckets struct {
			  Uri string `json:"uri"`
		  } `json:"buckets"`
}

type BucketConfigNodeExt struct {
	ThisNode bool `json:"thisNode"`
	Hostname string `json:"hostname"`
	Services map[string]int `json:"services"`
}

type TerseBucketConfig struct {
	SourceHost string
	Uuid string `json:"uuid"`
	Rev uint `json:"rev"`
	NodesExt []BucketConfigNodeExt `json:"nodesExt"`
}

func (config *TerseBucketConfig) GetSourceNodeExt() *BucketConfigNodeExt {
	for _, node := range config.NodesExt {
		if node.ThisNode {
			return &node
		}
	}

	return nil
}

type ClusterNode struct {
	Hostname string
	Services map[string]int
}

func ClusterNodesFromTerseBucketConfig(config TerseBucketConfig) []ClusterNode {
	var out []ClusterNode

	for _, node := range config.NodesExt {
		var newNode ClusterNode

		if node.Hostname == "" {
			newNode.Hostname = config.SourceHost
		} else {
			newNode.Hostname = node.Hostname
		}

		newNode.Services = node.Services

		out = append(out, newNode)
	}

	return out
}

func FetchHttpTerseBucketConfig(host string, port int, bucket, pass string) (TerseBucketConfig, error) {
	uri := fmt.Sprintf("http://%s:%d/pools/default/b/%s", host, port, bucket)

	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return TerseBucketConfig{}, err
	}

	req.SetBasicAuth(bucket, pass)

	var httpClient http.Client
	httpClient.Timeout = time.Millisecond * 2000

	resp, err := httpClient.Do(req)
	if err != nil {
		return TerseBucketConfig{}, err
	}

	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 {
			return TerseBucketConfig{}, errors.New("incorrect bucket/password")
		}

		return TerseBucketConfig{}, errors.New(fmt.Sprintf("http error (status code: %d)", resp.StatusCode))
	}

	configBytes, err := ioutil.ReadAll(resp.Body)

	configBytes = bytes.Replace(configBytes, []byte("$HOST"), []byte(host), -1)

	var config TerseBucketConfig
	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		return TerseBucketConfig{}, err
	}

	config.SourceHost = host

	return config, nil
}

func Diagnose(connStr, bucketPass string) {
	//======================================================================
	//  CONNECTION STRING
	//======================================================================
	gLog.Log("Parsing connection string `%s`", connStr)

	connSpec, err := connstr.Parse(connStr)
	if err != nil {
		gLog.Error("Failed to parse connection string of `%s` (error: %s)",
			connStr, err.Error())
	}

	connSpecSrv := connSpec.SrvRecordName()
	if connSpecSrv != "" {
		gLog.Log("Connection string was parsed as a potential DNS SRV record")
	}

	resConnSpec, err := connstr.Resolve(connSpec)
	if err != nil {
		gLog.Error("Failed to properly resolve connection string `%s` (error: %s)",
			connStr, err.Error())
	}

	if resConnSpec.UseSsl {
		gLog.Log("Connection string specifies to use secured connections")
	}

	gLog.Log("Connection string identifies the following CCCP endpoints:")
	for i, host := range resConnSpec.CccpHosts {
		gLog.Log("  %d. %s:%d", i+1, host.Host, host.Port)
	}

	gLog.Log("Connection string identifies the following HTTP endpoints:")
	for i, host := range resConnSpec.HttpHosts {
		gLog.Log("  %d. %s:%d", i+1, host.Host, host.Port)
	}

	gLog.Log("Connection string specifies bucket `%s`", resConnSpec.Bucket)


	//======================================================================
	//  DNS
	//======================================================================
	warnSingleHost := false
	if len(connSpec.Hosts) == 1 {
		warnSingleHost = true
	}

	if connSpecSrv != "" {
		_, srvAddrs, _ := net.LookupSRV("", "", connSpecSrv)
		aAddrs, _ := net.LookupHost(connSpec.Hosts[0].Host)

		if len(srvAddrs) > 0 {
			// Don't warn for single-hosts if using DNS SRV
			warnSingleHost = false
		}

		if len(srvAddrs) > 0 && len(aAddrs) > 0 {
			gLog.Warn(
				"The hostname specified in your connection string resolves both for SRV" +
				" records, as well as A records.  This is not suggested as later DNS" +
				" configuration changes could cause the wrong servers to be contacted")
		}
	}

	if warnSingleHost {
		gLog.Warn(
			"You're connection string specifies only a single host.  You should" +
			" consider adding additional static nodes from your cluster to this" +
			" list to improve your applications fault-tolerance")
	}

	for _, target := range connSpec.Hosts {

		gLog.Log("Performing DNS lookup for host `%s`", target.Host)

		addrs, err := net.LookupHost(target.Host)

		if err != nil {
			if dnsErr, ok := err.(*net.DNSError); ok {
				if dnsErr.Err == "no such host" {
					err = nil
					addrs = nil
				}
			} else {
				gLog.Error(
					"Failed to perform DNS lookup for bootstrap entry `%s` (error: %s)",
					err)
				continue
			}
		}

		if err != nil || len(addrs) == 0 {
			gLog.Error(
				"Bootstrap host `%s` does not have a valid DNS entry.",
				target.Host)
			continue
		} else if len(addrs) > 1 {
			gLog.Warn(
				"Bootstrap host `%s` has more than one single DNS entry associated.  While this" +
				" is not neccessarily an error, it has been known to cause difficult-to-diagnose" +
				" problems in the future when routing is changed or the cluster layout is updated.",
				target.Host)
		} else if addrs[0] != target.Host {
			gLog.Log(
				"Bootstrap host `%s` refers to a server with the address `%s`",
				target.Host, addrs[0])
		}
	}

	//======================================================================
	//  SSL
	//======================================================================
	if resConnSpec.UseSsl {
		gLog.Warn(
			"The FTS service within Couchbase Server is not currently capable" +
			" of serving data through SSL.  As this is the case, your application will" +
			" not be able to perform FTS queries with your SSL bootstrap configuration.")
	}


	//======================================================================
	//  BOOTSTRAP
	//======================================================================

	var nodesList []ClusterNode

	// Attempt to bootstrap via CCCP
	if nodesList == nil {
		if len(resConnSpec.CccpHosts) == 0 {
			gLog.Log("Not attempting CCCP, as the connection string does not support it")
		} else {
			gLog.Log("Attempting to connect to cluster via CCCP")

			gLog.Log("Failed to connect via CCCP, as it is not yet supported by the doctor")
		}
	}

	// Attempt to bootstrap via Terse HTTP endpoints
	if nodesList == nil {
		if len(resConnSpec.HttpHosts) == 0 {
			gLog.Log("Not attempting HTTP (Terse), as the connection string does not support it")
		} else {
			gLog.Log("Attempting to connect to cluster via HTTP (Terse)")

			var masterConfig *TerseBucketConfig

			for _, target := range resConnSpec.HttpHosts {
				gLog.Log("Attempting to fetch terse config via http from `%s:%d`", target.Host, target.Port)

				// Query the host
				config, err := FetchHttpTerseBucketConfig(target.Host, target.Port, resConnSpec.Bucket, bucketPass)
				if err != nil {
					gLog.Error(
						"Failed to fetch terse configuration via http from bootstrap host `%s` (error: %s)",
						target.Host, err.Error())

					continue
				}

				if masterConfig == nil {
					masterConfig = &config
				} else {
					if config.Uuid != masterConfig.Uuid {
						gLog.Error(
							"Boostrap host `%s` appears to be pointing to a different cluster.  Tests" +
							" will be running against the first successfully connected node in your" +
							" bootstrap list, as a client would behave.",
							target.Host)
					}
				}

				thisNodeExt := config.GetSourceNodeExt()
				if thisNodeExt.Hostname != "" && target.Host != thisNodeExt.Hostname {
					gLog.Warn(
						"Bootstrap host `%s` is not using the canonical node hostname of `%s`.  This" +
						" is not neccessarily an error, but has been known to result in strange and" +
						" difficult-to-diagnose errors in the future when routing gets changed.",
						target.Host, thisNodeExt.Hostname)
				}
			}

			if masterConfig != nil {
				nodesList = ClusterNodesFromTerseBucketConfig(*masterConfig)
			}
		}
	}

	// Attempt to bootstrap via full HTTP endpoints
	if nodesList == nil {
		if len(resConnSpec.HttpHosts) == 0 {
			gLog.Log("Not attempting HTTP (Full), as the connection string does not support it")
		} else {
			gLog.Log("Attempting to connect to cluster via HTTP (Full)")

			gLog.Log("Failed to connect via HTTP (Full), as it is not yet supported by the doctor")
		}
	}


	// Failed to bootstrap
	if nodesList == nil {
		gLog.Error("All endpoints specified by your connection string were unreachable, further cluster diagnostics are not possible")
		return
	}
	

	//======================================================================
	//  SERVICES
	//======================================================================
	for _, node := range nodesList {
		if !resConnSpec.UseSsl {
			if node.Services["kv"] != 0 {
				// TODO: Implement pinging of memcached services
				gLog.Log("KV service at `%s:%d` was not tested.  Not yet implemented.",
					node.Hostname, node.Services["kv"])
			}

			if node.Services["mgmt"] != 0 {
				uri := fmt.Sprintf("http://%s:%d/", node.Hostname, node.Services["mgmt"])
				_, err := gHttpClient.Get(uri)
				if err != nil {
					gLog.Error("Failed to connect to MGMT service at `%s:%d` (error: %s)",
						node.Hostname, node.Services["mgmt"], err.Error())
				} else {
					gLog.Log("Successfully connected to MGMT service at `%s:%d`",
						node.Hostname, node.Services["mgmt"])
				}
			}

			if node.Services["capi"] != 0 {
				uri := fmt.Sprintf("http://%s:%d/", node.Hostname, node.Services["capi"])
				_, err := gHttpClient.Get(uri)
				if err != nil {
					gLog.Error("Failed to connect to CAPI service at `%s:%d` (error: %s)",
						node.Hostname, node.Services["capi"], err.Error())
				} else {
					gLog.Log("Successfully connected to CAPI service at `%s:%d`",
						node.Hostname, node.Services["capi"])
				}
			}

			if node.Services["n1ql"] != 0 {
				uri := fmt.Sprintf("http://%s:%d/", node.Hostname, node.Services["n1ql"])
				_, err := gHttpClient.Get(uri)
				if err != nil {
					gLog.Error("Failed to connect to N1QL service at `%s:%d` (error: %s)",
						node.Hostname, node.Services["n1ql"], err.Error())
				} else {
					gLog.Log("Successfully connected to N1QL service at `%s:%d`",
						node.Hostname, node.Services["n1ql"])
				}
			}

			if node.Services["fts"] != 0 {
				uri := fmt.Sprintf("http://%s:%d/", node.Hostname, node.Services["fts"])
				_, err := gHttpClient.Get(uri)
				if err != nil {
					gLog.Error("Failed to connect to FTS service at `%s:%d` (error: %s)",
						node.Hostname, node.Services["fts"], err.Error())
				} else {
					gLog.Log("Successfully connected to FTS service at `%s:%d`",
						node.Hostname, node.Services["fts"])
				}
			}
		} else {
			gLog.Error("Testing of SSL connections is not yet supported")
		}
	}
}

