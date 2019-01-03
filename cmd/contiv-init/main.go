// Copyright (c) 2018 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ghodss/yaml"
	"github.com/nerdtakula/supervisor"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"

	"github.com/ligato/cn-infra/logging"
	"github.com/ligato/cn-infra/logging/logrus"
	"github.com/ligato/cn-infra/servicelabel"

	"fmt"
	"github.com/contiv/vpp/cmd/contiv-stn/model/stn"
	"github.com/contiv/vpp/pkg/pci"
	"github.com/contiv/vpp/plugins/contiv"
	"github.com/ligato/cn-infra/config"
	"github.com/ligato/cn-infra/db/keyval"
	"github.com/ligato/cn-infra/db/keyval/bolt"
	"github.com/ligato/cn-infra/db/keyval/etcd"
	"github.com/ligato/cn-infra/db/keyval/kvproto"
)

const (
	defaultContivCfgFile    = "/etc/agent/contiv.yaml"
	defaultEtcdCfgFile      = "/etc/etcd/etcd.conf"
	defaultBoltCfgFile      = "/etc/agent/bolt.conf"
	defaultSupervisorSocket = "/run/supervisor.sock"
	defaultStnServerSocket  = "/var/run/contiv/stn.sock"
	defaultCNISocketFile    = "/var/run/contiv/cni.sock"

	vppProcessName         = "vpp"
	contivAgentProcessName = "contiv-agent"

	etcdConnectionRetries = 20 // number of retries to connect to ETCD once STN is configured
)

var (
	contivCfgFile    = flag.String("contiv-config", defaultContivCfgFile, "location of the contiv-agent config file")
	etcdCfgFile      = flag.String("etcd-config", defaultEtcdCfgFile, "location of the ETCD config file")
	boltCfgFile      = flag.String("bolt-config", defaultBoltCfgFile, "location of the Bolt config file")
	supervisorSocket = flag.String("supervisor-socket", defaultSupervisorSocket, "management API socket file of the supervisor process")
	stnServerSocket  = flag.String("stn-server-socket", defaultStnServerSocket, "socket file where STN GRPC server listens for connections")
)

var logger logging.Logger // global logger

// init initializes the global logger
func init() {
	logger = logrus.DefaultLogger()
	logger.SetLevel(logging.DebugLevel)
}

// stealNIC requests stealing the specified NIC from the STN GRPC server.
func stealNIC(nicName string, useDHCP bool) (*stn.STNReply, error) {
	logger.Debugf("Stealing the NIC: %s", nicName)

	// connect to STN GRPC server
	conn, err := stnGrpcConnect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	c := stn.NewSTNClient(conn)

	// request stealing the interface
	reply, err := c.StealInterface(context.Background(), &stn.STNRequest{
		InterfaceName: nicName,
		DhcpEnabled:   useDHCP,
	})
	if err != nil {
		logger.Errorf("Error by executing STN GRPC: %v", err)
		return nil, err
	}

	logger.Debug(reply)
	return reply, nil
}

// releaseNIC requests reverting the specified NIC to its original state using the STN GRPC server.
func releaseNIC(nicName string, useDHCP bool) error {
	logger.Debugf("Releasing the NIC: %s", nicName)

	// connect to STN GRPC server
	conn, err := stnGrpcConnect()
	if err != nil {
		return err
	}
	defer conn.Close()
	c := stn.NewSTNClient(conn)

	// request release of the interface
	reply, err := c.ReleaseInterface(context.Background(), &stn.STNRequest{
		InterfaceName: nicName,
		DhcpEnabled:   useDHCP,
	})
	if err != nil {
		logger.Errorf("Error by executing STN GRPC: %v", err)
		return err
	}

	logger.Debug(reply)
	return nil
}

// grpcConnect connects to STN GRPC server.
func stnGrpcConnect() (*grpc.ClientConn, error) {
	dialer := grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout("unix", addr, timeout)
	})

	conn, err := grpc.Dial(*stnServerSocket, grpc.WithInsecure(), dialer)
	if err != nil {
		logger.Errorf("Unable to connect to STN GRPC: %v", err)
		return nil, err
	}

	return conn, nil
}

// parseConfig parses global Contiv config & node config from the file specified as an argument and optionally also from CRD.
func parseConfig() (config *contiv.Config, nodeConfig *contiv.NodeConfig, err error) {
	// read config YAML
	yamlFile, err := ioutil.ReadFile(*contivCfgFile)
	if err != nil {
		logger.Errorf("Error by reading config file %s: %v", *contivCfgFile, err)
		return
	}

	// unmarshal the YAML
	config = &contiv.Config{}
	err = yaml.Unmarshal(yamlFile, config)
	if err != nil {
		logger.Errorf("Error by unmarshalling YAML: %v", err)
		return
	}
	config.ApplyDefaults()

	// try to find node config
	nodeName := os.Getenv(servicelabel.MicroserviceLabelEnvVar)
	logger.Debugf("Looking for node '%s' specific config in ETCD/Bolt", nodeName)
	if nodeConfig = loadNodeConfigFromCRD(nodeName); nodeConfig != nil {
		// node configuration defined via CRD
		return
	}

	// node configuration not defined via CRD => search for node specific config inside
	// the configuration file
	logger.Debugf("Looking for node '%s' specific config inside the configuration file", nodeName)
	nodeConfig = config.GetNodeConfig(nodeName)

	return
}

// processSTNConfig looks up for STN configuration in the Contiv config ang node config.
// In case that STN was requested for this node, returns the interface to be stolen.
func processSTNConfig(config *contiv.Config, nodeConfig *contiv.NodeConfig) (nicToSteal string, useDHCP bool, err error) {

	// node config: interface name - priority 1
	if nodeConfig != nil {
		nicToSteal = nodeConfig.StealInterface
		if nicToSteal != "" {
			logger.Debugf("Found interface to be stolen: %s", nicToSteal)
			if nodeConfig.MainVPPInterface.UseDHCP == true {
				useDHCP = true
			}
		}
	}

	// global config: interface name - priority 2
	if nicToSteal == "" && config.StealInterface != "" {
		nicToSteal = config.StealInterface
		logger.Debugf("Found interface to be stolen: %s", nicToSteal)
	}

	// global config - first interface - priority 3
	if nicToSteal == "" && config.StealFirstNIC {
		// the first NIC will be stolen
		nicToSteal = getFirstInterfaceName()
		if nicToSteal != "" {
			logger.Infof("No specific NIC to steal specified, stealing the first one: %s", nicToSteal)
		}
	}

	useDHCP = useDHCP || config.IPAMConfig.NodeInterconnectDHCP
	return
}

// loadNodeConfigFromCRD loads node configuration defined via CRD, which was reflected
// into a remote kv-store by contiv-crd and possibly mirrored into local kv-store by contiv-agent.
func loadNodeConfigFromCRD(nodeName string) (nodeConfig *contiv.NodeConfig) {
	// try to connect to ETCD db
	etcdDB, err := etcdConnect()
	if err == nil {
		defer etcdDB.Close()
	}
	// try to open local Bolt db
	boltDB, err := boltOpen()
	if err == nil {
		defer boltDB.Close()
	}
	return contiv.LoadNodeConfigFromCRD(nodeName, etcdDB, boltDB, logger)
}

// getFirstInterfaceName returns the name of the first non-virtual Linux interface
func getFirstInterfaceName() string {
	// list existing links
	links, err := netlink.LinkList()
	if err != nil {
		logger.Error("Unable to list links:", err)
		return ""
	}

	// find link to steal
	for _, l := range links {
		if !strings.HasPrefix(l.Attrs().Name, "lo") &&
			!strings.HasPrefix(l.Attrs().Name, "vir") &&
			!strings.HasPrefix(l.Attrs().Name, "docker") {
			return l.Attrs().Name
		}
	}
	return ""
}

// etcdConnect connects to ETCD db.
func etcdConnect() (protoDb *kvproto.ProtoWrapper, err error) {
	etcdConfig := &etcd.Config{}

	// parse ETCD config file
	err = config.ParseConfigFromYamlFile(*etcdCfgFile, etcdConfig)
	if err != nil {
		logger.Errorf("Error by parsing config YAML file: %v", err)
		return nil, err
	}

	// prepare ETCD config
	etcdCfg, err := etcd.ConfigToClient(etcdConfig)
	if err != nil {
		logger.Errorf("Error by constructing ETCD config: %v", err)
		return nil, err
	}

	// connect in retry loop
	var conn *etcd.BytesConnectionEtcd
	for i := 0; i < etcdConnectionRetries; i++ {
		conn, err = etcd.NewEtcdConnectionWithBytes(*etcdCfg, logger)
		if err != nil {
			if i == etcdConnectionRetries-1 {
				logger.Errorf("Error by connecting to ETCD: %v", err)
				return nil, err
			}
			logger.Debugf("ETCD connection retry n. %d", i+1)
		} else {
			// connected
			break
		}
	}

	protoDb = kvproto.NewProtoWrapper(conn, &keyval.SerializerJSON{})
	return protoDb, nil
}

// boltOpen opens local Bolt db.
func boltOpen() (protoDb *kvproto.ProtoWrapper, err error) {
	boltConfig := &bolt.Config{}

	// parse Bolt config file
	err = config.ParseConfigFromYamlFile(*boltCfgFile, boltConfig)
	if err != nil {
		logger.Errorf("Error by parsing config YAML file: %v", err)
		return nil, err
	}

	// create bolt client
	client, err := bolt.NewClient(boltConfig)
	if err != nil {
		logger.Errorf("Error by creating Bolt client: %v", err)
		return nil, err
	}

	protoDb = kvproto.NewProtoWrapper(client, &keyval.SerializerJSON{})
	return protoDb, nil
}

// deriveVmxnet3PCI derives PCI address string from the vmxnet3 interface name
func deriveVmxnet3PCI(ifName string) (string, error) {
	var function, slot, bus, domain uint32
	numLen, err := fmt.Sscanf(ifName, "vmxnet3-%x/%x/%x/%x", &domain, &bus, &slot, &function)
	if err != nil {
		err = fmt.Errorf("cannot parse PCI address from the vmxnet3 interface name %s: %v", ifName, err)
		return "", err
	}
	if numLen != 4 {
		err = fmt.Errorf("cannot parse PCI address from the interface name %s: expected 4 address elements, received %d",
			ifName, numLen)
		return "", err
	}
	return fmt.Sprintf("%04x:%02x:%02x.%0x", domain, bus, slot, function), nil
}

func main() {
	flag.Parse()
	logger.Debugf("Starting contiv-init process")

	contivCfg, nodeCfg, err := parseConfig()
	if err != nil {
		logger.Errorf("Error by parsing config: %v", err)
		os.Exit(-1)
	}

	// check whether STN is required and get NIC name
	nicToSteal, useDHCP, err := processSTNConfig(contivCfg, nodeCfg)
	if err != nil {
		logger.Errorf("Error by processing STN config: %v", err)
		os.Exit(-1)
	}

	var stnData *stn.STNReply
	if nicToSteal != "" {
		// steal the NIC
		stnData, err = stealNIC(nicToSteal, useDHCP)
		if err != nil {
			logger.Warnf("Error by stealing the NIC %s: %v", nicToSteal, err)
			// do not fail of STN was not successful
			nicToSteal = ""
		}
	} else {
		logger.Debug("STN not requested")
	}

	// check if explicit bind to vfio-pci driver is needed (required for vmxnet3 interfaces)
	if nodeCfg != nil && strings.HasPrefix(nodeCfg.MainVPPInterface.InterfaceName, "vmxnet3-") {
		pciAddr, err := deriveVmxnet3PCI(nodeCfg.MainVPPInterface.InterfaceName)
		if err != nil {
			logger.Errorf("Error by deriving PCI address: %v", err)
			os.Exit(-1)
		}
		err = pci.DriverBind(pciAddr, "vfio-pci")
		if err != nil {
			logger.Errorf("Error binding to vfio-pci: %v", err)
			os.Exit(-1)
		}
	}

	// connect to supervisor API
	client := supervisor.New(*supervisorSocket, 0, "", "")

	// start VPP
	logger.Debug("Starting VPP")
	_, err = client.StartProcess(vppProcessName, false)
	if err != nil {
		logger.Errorf("Error by starting VPP process: %v", err)
		os.Exit(-1)
	}

	if nicToSteal != "" {
		// Check if the STN Daemon has been initialized
		if stnData == nil {
			logger.Errorf("STN configured in vswitch, but STN Daemon not initialized")
			os.Exit(-1)
		}

		// configure connectivity on VPP
		vppCfg, err := configureVpp(contivCfg, stnData, useDHCP)
		if err != nil {
			logger.Errorf("Error by configuring VPP: %v", err)
			client.StopProcess(vppProcessName, false)
			os.Exit(-1)
		}

		// persist VPP config in ETCD
		err = persistVppConfig(contivCfg, stnData, vppCfg, useDHCP)
		if err != nil {
			logger.Errorf("Error by persisting VPP config in ETCD: %v", err)
			client.StopProcess(vppProcessName, false)
			os.Exit(-1)
		}
	}

	// start contiv-agent
	logger.Debugf("Starting contiv-agent")
	// remove CNI server socket file
	// TODO: this should be done automatically by CNI-infra before socket bind, remove once implemented
	os.Remove(defaultCNISocketFile)
	_, err = client.StartProcess(contivAgentProcessName, false)
	if err != nil {
		logger.Errorf("Error by starting contiv-agent process: %v", err)
		client.StopProcess(vppProcessName, false)
		os.Exit(-1)
	}

	// wait until SIGINT/SIGTERM signal
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	logger.Debugf("%v signal received, exiting", sig)

	// request releasing the NIC
	if nicToSteal != "" {
		releaseNIC(nicToSteal, useDHCP)
	}
}
