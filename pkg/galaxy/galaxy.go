/*
 * Tencent is pleased to support the open source community by making TKEStack available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use
 * this file except in compliance with the License. You may obtain a copy of the
 * License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */
package galaxy

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	glog "k8s.io/klog"
	"tkestack.io/galaxy/pkg/api/docker"
	"tkestack.io/galaxy/pkg/galaxy/options"
	"tkestack.io/galaxy/pkg/gc"
	"tkestack.io/galaxy/pkg/network/kernel"
	"tkestack.io/galaxy/pkg/network/portmapping"
	"tkestack.io/galaxy/pkg/policy"
	"tkestack.io/galaxy/pkg/tke/eni"
)

type Galaxy struct {
	JsonConf
	*options.ServerRunOptions
	quitChan  chan struct{}
	dockerCli *docker.DockerInterface
	netConf   map[string]map[string]interface{}
	pmhandler *portmapping.PortMappingHandler
	client    kubernetes.Interface
	pm        *policy.PolicyManager
}

type JsonConf struct {
	NetworkConf     []map[string]interface{} // all detailed network configurations
	DefaultNetworks []string                 // pod's default networks if it doesn't have networks annotation
}

func NewGalaxy() *Galaxy {
	g := &Galaxy{
		ServerRunOptions: options.NewServerRunOptions(),
		quitChan:         make(chan struct{}),
		netConf:          map[string]map[string]interface{}{},
	}
	return g
}

func (g *Galaxy) Init() error {
	if g.JsonConfigPath == "" {
		return fmt.Errorf("json config is required")
	}
	data, err := ioutil.ReadFile(g.JsonConfigPath)
	if err != nil {
		return fmt.Errorf("read json config: %v", err)
	}
	if err := json.Unmarshal(data, &g.JsonConf); err != nil {
		return fmt.Errorf("bad config %s: %v", string(data), err)
	}
	glog.Infof("Json Config: %s", string(data))
	if err := g.checkNetworkConf(); err != nil {
		return err
	}
	dockerClient, err := docker.NewDockerInterface()
	if err != nil {
		return err
	}
	g.dockerCli = dockerClient
	g.pmhandler = portmapping.New("")
	return nil
}

func (g *Galaxy) checkNetworkConf() error {
	if len(g.NetworkConf) == 0 {
		return fmt.Errorf("empty network config")
	}
	if len(g.DefaultNetworks) == 0 {
		return fmt.Errorf("empty default networks")
	}
	for i := range g.NetworkConf {
		netConf := g.NetworkConf[i]
		if val, ok := netConf["type"]; !ok {
			return fmt.Errorf("bad network config %v, type is missing", netConf)
		} else if netType, ok := val.(string); !ok {
			return fmt.Errorf("bad network config %v, type is not string", netConf)
		} else {
			g.netConf[netType] = g.NetworkConf[i]
		}
	}
	for _, netType := range g.DefaultNetworks {
		if _, ok := g.netConf[netType]; !ok {
			return fmt.Errorf("network configuration is empty for default network %s", netType)
		}
	}
	return nil
}

func (g *Galaxy) Start() error {
	if err := g.Init(); err != nil {
		return err
	}
	g.initk8sClient()
	gc.NewFlannelGC(g.dockerCli, g.quitChan, g.cleanIPtables).Run()
	kernel.BridgeNFCallIptables(g.quitChan, g.BridgeNFCallIptables)
	kernel.IPForward(g.quitChan, g.IPForward)
	if err := g.setupIPtables(); err != nil {
		return err
	}
	if g.NetworkPolicy {
		g.pm = policy.New(g.client, g.quitChan)
		go wait.Until(g.pm.Run, 3*time.Minute, g.quitChan)
	}
	if g.RouteENI {
		kernel.DisableRPFilter(g.quitChan)
		eni.SetupENIs(g.quitChan)
	}
	return g.StartServer()
}

func (g *Galaxy) Stop() error {
	close(g.quitChan)
	g.quitChan = make(chan struct{})
	return nil
}

func (g *Galaxy) initk8sClient() {
	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		if g.Master == "" && g.KubeConf == "" {
			// galaxy currently not support running in pod, so either flagApiServer or flagKubeConf should be specified
			glog.Fatal("apiserver address unknown")
		}
		clientConfig, err = clientcmd.BuildConfigFromFlags(g.Master, g.KubeConf)
		if err != nil {
			glog.Fatalf("Invalid client config: error(%v)", err)
		}
	}
	clientConfig.QPS = 1000.0
	clientConfig.Burst = 2000
	glog.Infof("QPS: %e, Burst: %d", clientConfig.QPS, clientConfig.Burst)

	g.client, err = kubernetes.NewForConfig(clientConfig)
	if err != nil {
		glog.Fatalf("Can not generate client from config: error(%v)", err)
	}
	glog.Infof("apiserver address %s, kubeconf %s", g.Master, g.KubeConf)
}

func (g *Galaxy) SetClient(cli kubernetes.Interface) {
	g.client = cli
}
