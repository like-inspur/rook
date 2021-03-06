/*
Copyright 2016 The Rook Authors. All rights reserved.

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
package ceph

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"testing"

	etcd "github.com/coreos/etcd/client"

	clienttest "github.com/rook/rook/pkg/ceph/client/test"
	"github.com/rook/rook/pkg/ceph/mon"
	"github.com/rook/rook/pkg/ceph/osd"
	cephtest "github.com/rook/rook/pkg/ceph/test"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/clusterd/inventory"
	"github.com/rook/rook/pkg/util"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
)

// ************************************************************************************************
// ************************************************************************************************
//
// unit test functions
//
// ********************************"****************************************************************
// ************************************************************************************************
func TestCephLeaders(t *testing.T) {
	leader := newLeader("")

	nodes := make(map[string]*inventory.NodeConfig)
	inv := &inventory.Config{Nodes: nodes}
	nodes["a"] = &inventory.NodeConfig{PublicIP: "1.2.3.4"}

	etcdClient := util.NewMockEtcdClient()
	context := testContext()
	defer os.RemoveAll(context.ConfigDir)
	context.DirectContext = clusterd.DirectContext{EtcdClient: etcdClient, Inventory: inv}

	// mock the agent responses that the deployments were successful to start mons and osds
	etcdClient.WatcherResponses["/rook/_notify/a/monitor/status"] = "succeeded"
	etcdClient.WatcherResponses["/rook/_notify/b/monitor/status"] = "succeeded"
	etcdClient.WatcherResponses["/rook/_notify/a/osd/status"] = "succeeded"
	etcdClient.WatcherResponses["/rook/_notify/b/osd/status"] = "succeeded"

	// trigger a refresh event
	refresh := clusterd.NewRefreshEvent()
	refresh.Context = context
	leader.HandleRefresh(refresh)

	assert.True(t, etcdClient.GetChildDirs("/rook/services/ceph/osd/desired").Equals(util.CreateSet([]string{"a"})))
	assert.Equal(t, "mon0", etcdClient.GetValue("/rook/services/ceph/monitor/desired/a/id"))
	assert.Equal(t, "1.2.3.4", etcdClient.GetValue("/rook/services/ceph/monitor/desired/a/ipaddress"))
	assert.Equal(t, "6790", etcdClient.GetValue("/rook/services/ceph/monitor/desired/a/port"))

	// trigger an add node event
	nodes["b"] = &inventory.NodeConfig{PublicIP: "2.3.4.5"}
	refresh.NodesAdded.Add("b")
	leader.HandleRefresh(refresh)

	assert.True(t, etcdClient.GetChildDirs("/rook/services/ceph/osd/desired").Equals(util.CreateSet([]string{"a", "b"})))
	assert.NotEqual(t, "", etcdClient.GetValue("/rook/services/ceph/fsid"))
	assert.Equal(t, "adminsecret", etcdClient.GetValue("/rook/services/ceph/_secrets/admin"))
}

func TestOSDRefresh(t *testing.T) {
	leader := newLeader("")

	nodes := make(map[string]*inventory.NodeConfig)
	nodes["a"] = &inventory.NodeConfig{PublicIP: "1.2.3.4"}
	nodes["b"] = &inventory.NodeConfig{PublicIP: "2.2.3.4"}

	etcdClient := util.NewMockEtcdClient()
	context := testContext()
	defer os.RemoveAll(context.ConfigDir)
	context.DirectContext = clusterd.DirectContext{EtcdClient: etcdClient, Inventory: &inventory.Config{Nodes: nodes}}

	// mock the agent responses that the deployments were successful to start mons and osds
	etcdClient.WatcherResponses["/rook/_notify/a/monitor/status"] = "succeeded"
	etcdClient.WatcherResponses["/rook/_notify/b/monitor/status"] = "succeeded"
	etcdClient.WatcherResponses["/rook/_notify/a/osd/status"] = "succeeded"
	etcdClient.WatcherResponses["/rook/_notify/b/osd/status"] = "succeeded"

	assert.Equal(t, "", etcdClient.GetValue("/rook/services/ceph/osd/desired/a/ready"))
	assert.Equal(t, "", etcdClient.GetValue("/rook/services/ceph/osd/desired/b/ready"))

	// on first refresh we should trigger osds on all nodes, not just the node newly added
	refresh := clusterd.NewRefreshEvent()
	refresh.NodesAdded.Add("b")
	refresh.Context = context
	leader.HandleRefresh(refresh)

	assert.Equal(t, "1", etcdClient.GetValue("/rook/services/ceph/osd/desired/a/ready"))
	assert.Equal(t, "1", etcdClient.GetValue("/rook/services/ceph/osd/desired/b/ready"))
}

func TestExtractDesiredDeviceNode(t *testing.T) {
	// valid path with node id
	node, err := extractNodeIDFromDesiredDevice("/rook/services/ceph/osd/desired/abc/device/sdb")
	assert.Nil(t, err)
	assert.Equal(t, "abc", node)

	// node id not found
	key := "/rook/services/ceph/osd/desired"
	node, err = extractNodeIDFromDesiredDevice(key)
	assert.NotNil(t, err)
	assert.Equal(t, "", node)

	// ensure the handle device changed event can run without crashing
	response := &etcd.Response{Action: "create", Node: &etcd.Node{Key: key}}
	handleDeviceChanged(response, nil)

}

func TestRefreshKeys(t *testing.T) {
	leader := newLeader("")
	keys := leader.RefreshKeys()
	assert.Equal(t, 3, len(keys))
	assert.Equal(t, "/rook/services/ceph/osd/desired", keys[0].Path)
	assert.Equal(t, "/rook/services/ceph/fs/desired", keys[1].Path)
	assert.Equal(t, "/rook/services/ceph/object/desired", keys[2].Path)
}

func TestNewCephService(t *testing.T) {

	service := NewCephService("a,b,c", "", "", true, "root=default", "", osd.StoreConfig{}, "mynode")
	assert.NotNil(t, service)
	assert.Equal(t, "/rook/services/ceph/osd/desired", service.Leader.RefreshKeys()[0].Path)
	assert.Equal(t, 5, len(service.Agents))
	assert.Equal(t, "monitor", service.Agents[0].Name())
	assert.Equal(t, "cephmgr", service.Agents[1].Name())
	assert.Equal(t, "osd", service.Agents[2].Name())
	assert.Equal(t, "mds", service.Agents[3].Name())
	assert.Equal(t, "rgw", service.Agents[4].Name())
}

func TestCreateClusterInfo(t *testing.T) {
	// generate the secret key from the factory
	context := testContext()
	defer os.RemoveAll(context.ConfigDir)
	info, err := mon.CreateClusterInfo(context, "")
	assert.Nil(t, err)
	if info == nil {
		return
	}
	assert.NotEqual(t, "", info.FSID)
	assert.Equal(t, "adminsecret", info.AdminSecret)
	assert.Equal(t, "rookcluster", info.Name)

	// specify the desired secret key
	info, err = mon.CreateClusterInfo(context, "mysupersecret")
	assert.NotEqual(t, "", info.FSID)
	assert.Equal(t, "mysupersecret", info.AdminSecret)

}

func testContext() *clusterd.Context {

	configDir, _ := ioutil.TempDir("", "")
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(actionName string, command string, args ...string) (string, error) {
			logger.Infof("OUTPUT: %s %v", command, args)
			if command == "ceph-authtool" {
				cephtest.CreateClusterInfo(nil, path.Join(configDir, "rookcluster"), []string{"a"})
				return "mysecret", nil
			}
			return "", fmt.Errorf("unrecognized command: %s %+v", command, args)
		},
		MockExecuteCommandWithOutputFile: func(actionName, command, outfileArg string, args ...string) (string, error) {
			if command == "ceph" && args[0] == "mon_status" {
				return clienttest.MonInQuorumResponse(), nil
			}
			return "", fmt.Errorf("unrecognized command: %s %+v", command, args)
		},
	}
	return &clusterd.Context{Executor: executor, ConfigDir: configDir}
}
